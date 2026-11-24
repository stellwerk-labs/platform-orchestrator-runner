package rollout

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
)

// PostRepairAuthority contains public metadata only. The retained signed receipt
// owns the actor, exact failed revision and immutable execution target.
type PostRepairAuthority struct {
	ControlID string    `json:"control_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (a *PostRepairAuthority) UnmarshalJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return refused("post_authority_refused")
	}
	var value PostRepairAuthority
	seen := make(map[string]bool, 2)
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return refused("post_authority_refused")
		}
		seen[name] = true
		switch name {
		case "control_id":
			err = decoder.Decode(&value.ControlID)
		case "expires_at":
			err = decoder.Decode(&value.ExpiresAt)
		default:
			return refused("post_authority_refused")
		}
		if err != nil {
			return refused("post_authority_refused")
		}
	}
	if _, err = decoder.Token(); err != nil || len(seen) != 2 || decoder.Decode(new(any)) != io.EOF || !signedcontrol.UUID(value.ControlID) || value.ExpiresAt.IsZero() {
		return refused("post_authority_refused")
	}
	*a = value
	return nil
}

func (e *Engine) validatePostCommand(command Command) error {
	if err := e.scope(command); err != nil {
		return err
	}
	now := time.Now()
	if command.Version != NativePipelineVersion || command.IssuedAt.IsZero() || command.IssuedAt.After(now.Add(time.Minute)) || !command.ExpiresAt.After(command.IssuedAt) || command.ExpiresAt.Sub(command.IssuedAt) > 7*24*time.Hour {
		return refused("command_expired")
	}
	return e.validatePolicy(command)
}

func (e *Engine) recordPostRepair(ctx context.Context, tx *sql.Tx, p *Progress, command Command, request signedcontrol.Request) error {
	if p.Phase != "post_failed" || p.FailedStep != request.FailedStep {
		return refused("repair_refused")
	}
	if !e.repairablePostStep(command, request.FailedStep) {
		return refused("effects_require_reconciliation")
	}
	if err := e.validatePostCommand(command); err != nil {
		return err
	}
	if err := e.postApplyProof(ctx, tx, command, *p); err != nil {
		return err
	}
	p.PostRepair = &PostRepairAuthority{ControlID: request.ID, ExpiresAt: request.ExpiresAt}
	return e.recordRepairStep(ctx, tx, p, command, request.FailedStep)
}

// A progress boolean alone cannot prove that Apply committed its result/outputs.
func (e *Engine) postApplyProof(ctx context.Context, tx *sql.Tx, command Command, p Progress) error {
	var outputs []byte
	err := tx.QueryRowContext(ctx, `SELECT r.outputs FROM rollout_pipeline_results r JOIN rollout_effects e ON e.rollout=r.rollout AND e.step='apply' AND e.status='done' WHERE r.rollout=$1`, command.ID).Scan(&outputs)
	if err == sql.ErrNoRows || !p.InfrastructureApplied {
		return refused("effects_require_reconciliation")
	}
	if err != nil {
		return err
	}
	if _, err = e.decrypt(outputs); err != nil {
		return refused("post_outputs_unavailable")
	}
	rows, err := tx.QueryContext(ctx, `SELECT step FROM rollout_effects WHERE rollout=$1 AND step<>'apply' AND status<>'done'`, command.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var effect string
		if err = rows.Scan(&effect); err != nil {
			return err
		}
		if len(effect) < 6 || effect[:5] != "task:" {
			return refused("effects_require_reconciliation")
		}
		tool, ok := e.toolForStep(command, effect[5:])
		if !ok || tool.Phase != "post_apply" || tool.Retry == "unsafe" {
			return refused("effects_require_reconciliation")
		}
	}
	return rows.Err()
}

type postAttemptKey struct{}
type postAttempt struct {
	command Command
	parent  context.Context
	run     bool
}

func postDeadline(command Command, p Progress, now time.Time) (time.Time, error) {
	if command.ExecutionLimits == nil || !command.ExecutionLimits.valid() {
		return time.Time{}, refused("execution_limits_refused")
	}
	expiry := command.ExpiresAt
	if p.PostRepair != nil {
		expiry = p.PostRepair.ExpiresAt
	}
	if !expiry.After(now) {
		if p.PostRepair != nil {
			return time.Time{}, refused("post_repair_expired")
		}
		return time.Time{}, refused("post_authority_expired")
	}
	deadline := now.Add(time.Duration(command.ExecutionLimits.TaskSeconds) * time.Second)
	if expiry.Before(deadline) {
		deadline = expiry
	}
	return deadline, nil
}

func (e *Engine) postStageContext(parent context.Context, id string) (context.Context, context.CancelFunc, error) {
	readCtx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	tx, err := e.beginCommand(readCtx, id)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	command, err := e.commandInTransaction(readCtx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	p, err := readProgress(readCtx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	deadline, run, err := e.enterNativePost(readCtx, tx, command, &p)
	if err != nil {
		return nil, nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	attempt := postAttempt{command: command, parent: parent, run: run}
	ctx := context.WithValue(parent, postAttemptKey{}, attempt)
	bounded, stop := context.WithDeadline(ctx, deadline)
	return bounded, stop, nil
}

func (e *Engine) enterNativePost(ctx context.Context, tx *sql.Tx, command Command, p *Progress) (time.Time, bool, error) {
	bookkeeping := time.Now().Add(15 * time.Second)
	if terminal(p.Phase) || p.Phase == "post_failed" {
		return bookkeeping, false, nil
	}
	if p.Phase != "applied" && p.Phase != "post_retrying" && p.Phase != "post_tasks" {
		return time.Time{}, false, refused("post_authority_refused")
	}
	if err := e.validatePostCommand(command); err != nil {
		return time.Time{}, false, err
	}
	if err := e.postApplyProof(ctx, tx, command, *p); err != nil {
		return time.Time{}, false, err
	}
	next, tool, err := e.nextPostStep(ctx, tx, command, *p)
	if err != nil {
		return time.Time{}, false, err
	}
	if next == "" {
		p.Phase, p.FailedStep, p.ErrorCode = "completed", "", ""
		if _, err = tx.ExecContext(ctx, `DELETE FROM rollout_environments WHERE rollout=$1`, command.ID); err != nil {
			return time.Time{}, false, err
		}
		return bookkeeping, false, saveProgress(ctx, tx, p)
	}
	deadline, err := postDeadline(command, *p, time.Now())
	if err != nil {
		var code *Error
		if !errors.As(err, &code) || code.Code == "execution_limits_refused" {
			return time.Time{}, false, err
		}
		if tool.Retry != "receipt" && tool.Retry != "read_only" {
			return time.Time{}, false, refused("effects_require_reconciliation")
		}
		p.Phase, p.FailedStep, p.ErrorCode = "post_failed", next, code.Code
		return bookkeeping, false, saveProgress(ctx, tx, p)
	}
	p.Phase, p.ErrorCode = "post_tasks", ""
	return deadline, true, saveProgress(ctx, tx, p)
}

func (e *Engine) nextPostStep(ctx context.Context, tx *sql.Tx, command Command, p Progress) (string, rolloutexec.Tool, error) {
	for _, step := range command.Steps {
		tool, ok := e.toolForStep(command, step.ID)
		if !ok || tool.Phase != "post_apply" || successfulStep(p, step.ID) {
			continue
		}
		var done bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rollout_effects WHERE rollout=$1 AND step=$2 AND status='done')`, command.ID, "task:"+step.ID).Scan(&done); err != nil {
			return "", tool, err
		}
		if !done {
			return step.ID, tool, nil
		}
	}
	return "", rolloutexec.Tool{}, nil
}

func successfulStep(p Progress, id string) bool {
	for _, result := range p.Steps {
		if result.ID == id && result.Status == "ok" {
			return true
		}
	}
	return false
}

func (e *Engine) nativePost(ctx context.Context, id string) error {
	attempt, ok := ctx.Value(postAttemptKey{}).(postAttempt)
	if !ok || attempt.command.ID != id {
		return refused("post_authority_refused")
	}
	if !attempt.run {
		return nil
	}
	if err := e.tasks(ctx, id, "post_apply"); err != nil {
		return err
	}
	live, cancel := context.WithTimeout(attempt.parent, 15*time.Second)
	defer cancel()
	p, err := e.Get(live, id)
	if err != nil || p.Phase == "post_failed" || p.Phase == "post_retrying" {
		return err
	}
	return e.finish(live, id, "completed", "")
}

func (e *Engine) nativePostTasks(ctx context.Context, id string) error {
	attempt, ok := ctx.Value(postAttemptKey{}).(postAttempt)
	if !ok || !attempt.run || attempt.command.ID != id {
		return refused("post_authority_refused")
	}
	command := attempt.command
	if err := e.validatePostCommand(command); err != nil {
		return err
	}
	live, cancel := context.WithTimeout(attempt.parent, 15*time.Second)
	review, outputs, err := e.taskInputs(live, id)
	cancel()
	if err != nil {
		return err
	}
	for _, step := range command.Steps {
		tool, ok := e.toolForStep(command, step.ID)
		if !ok || tool.Phase != "post_apply" {
			continue
		}
		p, readErr := e.postTaskProgress(ctx, id)
		if readErr != nil {
			return readErr
		}
		if p.Phase == "post_failed" || p.Phase == "post_retrying" {
			return nil
		}
		if p.Phase != "post_tasks" {
			return refused("post_authority_refused")
		}
		if successfulStep(p, step.ID) {
			continue
		}
		if failure := postTaskDeadline(ctx, tool, step.ID); failure != nil {
			return failure
		}
		if err = e.task(ctx, command, step, tool, review, outputs); err != nil {
			if failure := postTaskDeadline(ctx, tool, step.ID); failure != nil {
				return failure
			}
			return err
		}
	}
	return nil
}

func (e *Engine) postTaskProgress(ctx context.Context, id string) (Progress, error) {
	attempt, ok := ctx.Value(postAttemptKey{}).(postAttempt)
	if !ok || !attempt.run || attempt.command.ID != id {
		return Progress{}, refused("post_authority_refused")
	}
	live, cancel := context.WithTimeout(attempt.parent, 15*time.Second)
	defer cancel()
	return e.Get(live, id)
}

func postTaskDeadline(ctx context.Context, tool rolloutexec.Tool, step string) error {
	if ctx.Err() == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && (tool.Retry == "receipt" || tool.Retry == "read_only") {
		return &operationDeadline{phase: "post-tasks", step: step, retry: tool.Retry}
	}
	return ctx.Err()
}

func (e *Engine) repairablePostStep(command Command, step string) bool {
	tool, ok := e.toolForStep(command, step)
	return ok && tool.Phase == "post_apply" && (tool.Retry == "receipt" || tool.Retry == "read_only")
}
