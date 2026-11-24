package rollout

import (
	"context"
	"database/sql"
	"errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

func (e *Engine) tasks(ctx context.Context, id, phase string) error {
	if e.config.Postgres != nil && phase == "post_apply" {
		return e.nativePostTasks(ctx, id)
	}
	command, err := e.checkedCommand(ctx, id)
	if err != nil {
		return err
	}
	var review, outputs []byte
	if phase != "pre_plan" {
		review, outputs, err = e.taskInputs(ctx, id)
		if err != nil {
			return err
		}
	}
	for _, step := range command.Steps {
		tool, ok := e.toolForStep(command, step.ID)
		if !ok || tool.Phase != phase {
			continue
		}
		if err = e.task(ctx, command, step, tool, review, outputs); err != nil {
			return err
		}
		p, err := e.Get(ctx, id)
		if err != nil || p.Phase == "post_failed" {
			return err
		}
	}
	return nil
}

func (e *Engine) taskInputs(ctx context.Context, id string) ([]byte, []byte, error) {
	var review, outputs []byte
	err := e.db.QueryRowContext(ctx, `SELECT review,outputs FROM rollout_artifacts WHERE rollout=$1`, id).Scan(&review, &outputs)
	if errors.Is(err, sql.ErrNoRows) {
		err = e.db.QueryRowContext(ctx, `SELECT outputs FROM rollout_pipeline_results WHERE rollout=$1`, id).Scan(&outputs)
	}
	if err != nil {
		return nil, nil, err
	}
	plainReview, err := e.decrypt(review)
	if err != nil {
		return nil, nil, err
	}
	plainOutputs, err := e.decrypt(outputs)
	return plainReview, plainOutputs, err
}

func (e *Engine) task(ctx context.Context, command Command, step rolloutexec.Step, tool rolloutexec.Tool, review, outputs []byte) error {
	pending, err := e.pendingTask(ctx, command.ID, step.ID, tool)
	if err != nil || !pending {
		if command.Version == NativePipelineVersion && tool.Phase == "post_apply" {
			if failure := postTaskDeadline(ctx, tool, step.ID); failure != nil {
				return failure
			}
		}
		return err
	}
	result, err := e.executor.RunStep(ctx, command.Request, step, review, outputs)
	if command.Version == NativePipelineVersion && tool.Phase == "post_apply" {
		if failure := postTaskDeadline(ctx, tool, step.ID); failure != nil {
			return failure
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return e.taskFailed(ctx, command.ID, step.ID, tool, "task_failed")
	}
	switch result.Decision {
	case "allow":
		return e.recordStep(ctx, command.ID, StepResult{ID: step.ID, Phase: tool.Phase, Status: "ok", Outcome: taskOutcome(step, tool, "allow")})
	case "deny":
		if err = e.recordStep(ctx, command.ID, StepResult{ID: step.ID, Phase: tool.Phase, Status: "denied", Code: "check_denied", Outcome: taskOutcome(step, tool, "deny")}); err != nil {
			return err
		}
		return refused("check_denied")
	default:
		if result.Category == "unknown_outcome" {
			return refused("task_outcome_unknown")
		}
		return e.taskFailed(ctx, command.ID, step.ID, tool, "task_failed")
	}
}

func (e *Engine) pendingTask(ctx context.Context, id, step string, tool rolloutexec.Tool) (bool, error) {
	if e.stopped.Load() {
		return false, errStorageUnavailable
	}
	p, err := e.Get(ctx, id)
	if err != nil {
		return false, err
	}
	if terminal(p.Phase) {
		return false, refused("rollout_stopped")
	}
	if e.config.Postgres != nil && tool.Phase == "post_apply" && p.Phase != "post_tasks" {
		return false, refused("post_authority_refused")
	}
	for _, result := range p.Steps {
		if result.ID == step && result.Status == "ok" {
			return false, nil
		}
	}
	if tool.Retry == "read_only" {
		return true, nil
	}
	status, err := e.claim(ctx, id, "task:"+step, false)
	if err != nil {
		return false, err
	}
	switch status {
	case "done":
		return false, nil
	case "new":
		return true, nil
	default:
		return false, e.taskFailed(ctx, id, step, tool, "task_interrupted")
	}
}

func (e *Engine) taskFailed(ctx context.Context, id, step string, tool rolloutexec.Tool, code string) error {
	if err := e.recordStep(ctx, id, StepResult{ID: step, Phase: tool.Phase, Status: "failed", Code: code}); err != nil {
		return err
	}

	if tool.Phase != "post_apply" {
		_, _ = e.update(ctx, id, func(p *Progress) error {
			p.FailedStep = step
			return nil
		})
		return refused(code)
	}
	if tool.Retry == "unsafe" {
		return refused("task_outcome_unknown")
	}
	_, err := e.update(ctx, id, func(p *Progress) error {
		p.Phase, p.ErrorCode, p.FailedStep = "post_failed", code, step
		return nil
	})
	return err
}

func (e *Engine) recordStep(ctx context.Context, id string, result StepResult) error {
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := readProgress(ctx, tx, id)
	if err != nil {
		return err
	}
	if terminal(p.Phase) {
		return refused("rollout_stopped")
	}
	matched := false
	for i := range p.Steps {
		if p.Steps[i].ID == result.ID {
			p.Steps[i], matched = result, true
		}
	}
	if !matched {
		p.Steps = append(p.Steps, result)
	}
	if result.Status == "ok" {
		if _, err = tx.ExecContext(ctx, `UPDATE rollout_effects SET status='done' WHERE rollout=$1 AND step=$2`, id, "task:"+result.ID); err != nil {
			return err
		}
	}
	if err = saveProgress(ctx, tx, &p); err != nil {
		return err
	}
	return tx.Commit()
}

// Outcome text describes the frozen trusted tool, never arbitrary process output.
func taskOutcome(step rolloutexec.Step, tool rolloutexec.Tool, verdict string) *StepOutcome {
	meaning := "Task completed"
	if tool.Phase == "pre_plan" {
		meaning = "Preparation check passed"
	}
	if tool.Phase == "plan_check" {
		meaning = "Plan satisfies this check"
	}
	if verdict == "deny" {
		meaning = "Plan was denied by this check"
	}
	return &StepOutcome{Verdict: verdict, Meaning: meaning, ToolID: step.ToolID, ToolName: tool.Definition.Name, ToolVersion: tool.Definition.Version}
}
