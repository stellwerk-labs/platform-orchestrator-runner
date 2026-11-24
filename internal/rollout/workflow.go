package rollout

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

// References, never private plans/configuration/outputs, enter DBOS checkpoints.
type reference struct {
	ID         string `json:"id"`
	Phase      string `json:"phase"`
	FailedStep string `json:"failed_step,omitempty"`
	Subject    string `json:"subject,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	GateWait   int64  `json:"gate_wait_ns,omitempty"`
}

func (e *Engine) execute(ctx dbos.Context, id string) (result reference, resultErr error) {
	defer func() {
		if interrupted := e.interrupted(); interrupted != nil {
			resultErr = interrupted
		}
	}()
	var err error
	ctx, err = e.enterWorkflow(ctx, id)
	if err != nil {
		if interrupted := e.interrupted(); interrupted != nil {
			return reference{ID: id}, interrupted
		}
		return reference{ID: id}, err
	}
	command, err := e.command(ctx, id)
	if err != nil {
		return reference{ID: id}, err
	}
	if command.Strategy == "observation" {
		return e.executeObservation(ctx, id)
	}
	if command.Recipe != nil && command.Strategy == "apply_saved" {
		return e.executeRecipeSaved(ctx, id)
	}
	if command.Version == e.pipelineVersion() && command.Strategy != "reviewed" {
		return e.executePipeline(ctx, id, command.Strategy)
	}
	return e.executeReviewed(ctx, id)
}

func (e *Engine) executeReviewed(ctx dbos.Context, id string) (reference, error) {
	var result reference
	var err error
	for _, stage := range []struct {
		name string
		run  func(context.Context, string) error
	}{{"preparation", e.prepare}, {"plan", e.plan}, {"checks", e.check}, {"approval-ready", e.approvalReady}} {
		result, err = e.stage(ctx, id, stage.name, stage.run)
		if err != nil || terminal(result.Phase) {
			return result, err
		}
	}
	if result.Phase != "approved" {
		if err = e.waitApproval(ctx, result); err != nil {
			return result, err
		}
	}

	result, err = e.stage(ctx, id, "apply", e.apply)
	if err != nil || terminal(result.Phase) {
		return result, err
	}
	return e.executePostTasks(ctx, id)
}

func (e *Engine) waitApproval(ctx dbos.Context, result reference) error {
	_, err := dbos.Recv[string](ctx, "approval", time.Duration(result.GateWait))
	if err != nil && ctx.Err() == nil {
		_ = e.finish(ctx, result.ID, "failed", "approval_wait_expired")
	}
	return err
}

func (e *Engine) stage(ctx dbos.Context, id, name string, run func(context.Context, string) error) (reference, error) {
	return dbos.RunAsStep(ctx, func(stepCtx context.Context) (result reference, resultErr error) {
		defer func() {
			if interrupted := e.interrupted(); interrupted != nil {
				resultErr = interrupted
			}
		}()
		if err := e.runStage(stepCtx, id, name, run); err != nil {
			return reference{ID: id}, err
		}
		return e.stageReference(stepCtx, id, name)
	}, dbos.WithStepName(name), dbos.WithStepMaxRetries(0))
}

func (e *Engine) runStage(ctx context.Context, id, name string, run func(context.Context, string) error) error {
	if err := e.admitStage(); err != nil {
		return err
	}
	defer e.releaseStage()
	bounded, cancel, err := e.stageContext(ctx, id, name)
	if err == nil {
		defer cancel()
		err = run(bounded, id)
	}
	if interrupted := e.interrupted(); interrupted != nil {
		return interrupted
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var deadline *operationDeadline
	if e.config.Postgres != nil && errors.As(err, &deadline) {
		return e.recordOperationDeadline(ctx, id, deadline)
	}
	code := "operation_failed"
	var safe *Error
	if errors.As(err, &safe) {
		code = safe.Code
	}
	phase := "failed"
	if code == "apply_outcome_unknown" || code == "task_outcome_unknown" {
		phase = "outcome_unknown"
	}
	if err = e.finish(ctx, id, phase, code); err != nil {
		return refused("storage_unavailable")
	}
	return nil
}

func (e *Engine) stageReference(ctx context.Context, id, name string) (reference, error) {
	if interrupted := e.interrupted(); interrupted != nil {
		return reference{ID: id}, interrupted
	}
	p, err := e.Get(ctx, id)
	if err != nil {
		return reference{ID: id}, refused("storage_unavailable")
	}
	result := reference{ID: id, Phase: p.Phase, FailedStep: p.FailedStep, Subject: p.Subject, ErrorCode: p.ErrorCode}
	if name != "approval-ready" {
		return result, nil
	}
	command, err := e.command(ctx, id)
	if err != nil {
		return result, refused("storage_unavailable")
	}
	result.GateWait = int64(max(time.Until(command.ExpiresAt), time.Millisecond))
	// Local approval consumes its durable notification even when approval arrived
	// before this readiness checkpoint committed.
	if (command.Approval.Mode == "local" || command.Approval.Mode == "cost_non_increasing" && (p.ApprovalDecision == nil || p.ApprovalDecision.Source == "human")) && !terminal(result.Phase) {
		result.Phase = "awaiting_approval"
	}
	return result, nil
}

func (e *Engine) phase(ctx context.Context, id, phase string) error {
	_, err := e.update(ctx, id, func(p *Progress) error {
		if terminal(p.Phase) {
			return refused("rollout_stopped")
		}
		p.Phase, p.ErrorCode = phase, ""
		return nil
	})
	return err
}

func (e *Engine) checkedCommand(ctx context.Context, id string) (Command, error) {
	command, err := e.command(ctx, id)
	if err == nil {
		err = e.validate(command)
	}
	return command, err
}

func (e *Engine) prepare(ctx context.Context, id string) error {
	if err := e.phase(ctx, id, "preparing"); err != nil {
		return err
	}
	return e.tasks(ctx, id, "pre_plan")
}

func (e *Engine) check(ctx context.Context, id string) error {
	if err := e.phase(ctx, id, "checking"); err != nil {
		return err
	}
	return e.tasks(ctx, id, "plan_check")
}

func (e *Engine) plan(ctx context.Context, id string) error {
	command, err := e.checkedCommand(ctx, id)
	if err != nil {
		return err
	}
	var existing string
	err = e.db.QueryRowContext(ctx, `SELECT digest FROM rollout_artifacts WHERE rollout=$1`, id).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = e.phase(ctx, id, "planning"); err != nil {
		return err
	}
	plan, err := e.executor.Plan(ctx, command.Request)
	if failure := e.deadlineFailure(ctx, "plan", "plan_failed", plan.Logs, plan.LogsTooLarge); failure != nil {
		return failure
	}
	if err != nil {
		if saveErr := e.saveFailedPlanLogs(ctx, id, plan); saveErr != nil {
			return saveErr
		}
		if plan.LogsTooLarge {
			return refused("plan_logs_too_large")
		}
		return refused("plan_failed")
	}
	if command.Cost != nil && command.Cost.Enabled {
		cost := e.executor.EstimateCosts(ctx, plan.Review)
		plan.Cost = &cost
	}
	if failure := e.deadlineFailure(ctx, "plan", "plan_failed", plan.Logs, plan.LogsTooLarge); failure != nil {
		return failure
	}
	return e.savePlan(ctx, command, plan)
}

func (e *Engine) savePlan(ctx context.Context, command Command, plan rolloutexec.Plan) error {
	id := command.ID
	review, err := e.encrypt(plan.Review)
	if err != nil {
		return err
	}
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := readProgress(ctx, tx, command.ID)
	if err != nil || terminal(p.Phase) {
		return refused("rollout_stopped")
	}
	p.Phase, p.Changes, p.PlanChanges = "planned", &plan.Changes, &plan.Changes
	var commandDigest string
	if err = tx.QueryRowContext(ctx, `SELECT digest FROM rollout_commands WHERE id=$1`, command.ID).Scan(&commandDigest); err != nil {
		return err
	}
	costDigest, err := e.insertCost(ctx, tx, command.ID, plan.Cost)
	if err != nil {
		return err
	}
	p.Cost = plan.Cost
	p.Subject = reviewSubject(commandDigest, plan.Ciphertext, plan.Review, plan.Logs, costDigest)
	if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_artifacts(rollout,plan,review,digest) VALUES($1,$2,$3,$4)`, command.ID, plan.Ciphertext, review, digest(plan.Ciphertext)); err != nil {
		return err
	}
	if command.Strategy == "plan" {
		result, resultErr := rolloutexec.Result(command.Request, plan.PlannedOutputs, &plan.Changes, true)
		if resultErr != nil {
			return resultErr
		}
		if err = e.insertPipelineResult(ctx, tx, command.ID, result, plan.PlannedOutputs); err != nil {
			return err
		}
	}
	attachment, err := e.insertPlanLogs(ctx, tx, command, &p, plan)
	if err != nil {
		return err
	}
	if command.NativeTarget != nil {
		err = e.saveLogCapture(ctx, tx, &p, *p.LogCapture, attachment)
	} else {
		err = saveProgress(ctx, tx, &p)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) apply(ctx context.Context, id string) error {
	command, err := e.checkedCommand(ctx, id)
	if err != nil {
		return err
	}
	if command.Version == e.pipelineVersion() {
		return e.reviewedApply(ctx, id, command)
	}
	status, err := e.claim(ctx, id, "apply", true)
	if err != nil {
		return err
	}
	if status == "done" {
		return nil
	}
	if status != "new" {
		return refused("apply_outcome_unknown")
	}
	var ciphertext []byte
	var expected string
	if err = e.db.QueryRowContext(ctx, `SELECT plan,digest FROM rollout_artifacts WHERE rollout=$1`, id).Scan(&ciphertext, &expected); err != nil {
		return refused("apply_outcome_unknown")
	}
	if digest(ciphertext) != expected {
		return refused("apply_outcome_unknown")
	}
	outputs, err := e.executor.Apply(ctx, command.Request, ciphertext, expected)
	if err != nil {
		return refused("apply_outcome_unknown")
	}
	return e.saveApplyOutputs(ctx, id, outputs)
}

func (e *Engine) saveApplyOutputs(ctx context.Context, id string, outputs []byte) error {
	encrypted, err := e.encrypt(outputs)
	if err != nil {
		return refused("apply_outcome_unknown")
	}
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return refused("apply_outcome_unknown")
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE rollout_artifacts SET outputs=$1 WHERE rollout=$2`, encrypted, id); err != nil {
		return refused("apply_outcome_unknown")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE rollout_effects SET status='done' WHERE rollout=$1 AND step='apply'`, id); err != nil {
		return refused("apply_outcome_unknown")
	}
	p, err := readProgress(ctx, tx, id)
	if err != nil {
		return err
	}
	p.Phase = "applied"
	if err = saveProgress(ctx, tx, &p); err != nil {
		return refused("apply_outcome_unknown")
	}
	return tx.Commit()
}

func (e *Engine) claim(ctx context.Context, id, step string, apply bool) (string, error) {
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM rollout_effects WHERE rollout=$1 AND step=$2`, id, step).Scan(&status)
	if err == nil {
		return status, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	p, err := readProgress(ctx, tx, id)
	if err != nil {
		return "", err
	}
	direct := false
	if apply {
		command, readErr := e.commandInTransaction(ctx, tx, id)
		if readErr != nil {
			return "", readErr
		}
		direct = command.Version == e.pipelineVersion() && (command.Strategy == "apply" || command.Strategy == "destroy" || command.Strategy == "apply_saved") && p.Phase == "preparing"
	}
	if terminal(p.Phase) || (apply && p.Phase != "approved" && !direct) {
		return "", refused("approval_required")
	}
	if apply {
		p.Phase = "applying"
		if err = saveProgress(ctx, tx, &p); err != nil {
			return "", err
		}
	}
	err = e.insertClaim(ctx, tx, id, step)
	if err != nil {
		return "", err
	}
	return "new", tx.Commit()
}

func (e *Engine) post(ctx context.Context, id string) error {
	if e.config.Postgres != nil {
		return e.nativePost(ctx, id)
	}
	if err := e.phase(ctx, id, "post_tasks"); err != nil {
		return err
	}
	if err := e.tasks(ctx, id, "post_apply"); err != nil {
		return err
	}
	p, err := e.Get(ctx, id)
	if err != nil || p.Phase == "post_failed" {
		return err
	}
	return e.finish(ctx, id, "completed", "")
}

func (e *Engine) commandInTransaction(ctx context.Context, tx *sql.Tx, id string) (Command, error) {
	return e.loadCommand(ctx, tx, id)
}

func (e *Engine) toolForStep(command Command, id string) (rolloutexec.Tool, bool) {
	for _, step := range command.Steps {
		if step.ID == id {
			for _, tool := range e.executor.Tools {
				if tool.ID == step.ToolID {
					return tool, true
				}
			}
		}
	}
	return rolloutexec.Tool{}, false
}
