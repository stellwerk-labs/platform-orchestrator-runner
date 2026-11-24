package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

// ExecutionLimits are per-attempt ceilings inside the signed command lifetime.
type ExecutionLimits struct {
	PlanSeconds  int64 `json:"plan_seconds"`
	ApplySeconds int64 `json:"apply_seconds"`
	TaskSeconds  int64 `json:"task_seconds"`
}

func (v ExecutionLimits) valid() bool {
	return v.PlanSeconds > 0 && v.PlanSeconds <= 14400 &&
		v.ApplySeconds > 0 && v.ApplySeconds <= 86400 &&
		v.TaskSeconds > 0 && v.TaskSeconds <= 14400
}

func (v *ExecutionLimits) UnmarshalJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return refused("execution_limits_refused")
	}
	var limits ExecutionLimits
	fields := map[string]*int64{"plan_seconds": &limits.PlanSeconds, "apply_seconds": &limits.ApplySeconds, "task_seconds": &limits.TaskSeconds}
	seen := make(map[string]bool, len(fields))
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		field := fields[name]
		if err != nil || !ok || field == nil || seen[name] || decoder.Decode(field) != nil {
			return refused("execution_limits_refused")
		}
		seen[name] = true
	}
	if _, err = decoder.Token(); err != nil || len(seen) != len(fields) || decoder.Decode(new(any)) != io.EOF || !limits.valid() {
		return refused("execution_limits_refused")
	}
	*v = limits
	return nil
}

func nativeStageDeadline(command Command, name string, now time.Time) (time.Time, error) {
	if command.ExecutionLimits == nil || !command.ExecutionLimits.valid() {
		return time.Time{}, refused("execution_limits_refused")
	}
	if !command.ExpiresAt.After(now) {
		return time.Time{}, refused("command_expired")
	}
	var seconds int64
	switch name {
	case "plan", "observation":
		seconds = command.ExecutionLimits.PlanSeconds
	case "apply", "direct-apply", "saved-apply":
		seconds = command.ExecutionLimits.ApplySeconds
	case "preparation", "checks", "post-tasks":
		seconds = command.ExecutionLimits.TaskSeconds
	case "approval-ready", "plan-complete", "saved-ready", "observation-complete":
		seconds = 120
	default:
		return time.Time{}, refused("execution_limits_refused")
	}
	deadline := now.Add(time.Duration(seconds) * time.Second)
	if command.ExpiresAt.Before(deadline) {
		deadline = command.ExpiresAt
	}
	return deadline, nil
}

func (e *Engine) stageContext(ctx context.Context, id, name string) (context.Context, context.CancelFunc, error) {
	if e.config.Postgres == nil {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
		return bounded, cancel, nil
	}
	if name == "post-tasks" {
		return e.postStageContext(ctx, id)
	}
	readCtx, cancelRead := context.WithTimeout(ctx, 15*time.Second)
	command, err := e.command(readCtx, id)
	cancelRead()
	if err != nil {
		return nil, nil, err
	}
	deadline, err := nativeStageDeadline(command, name, time.Now())
	if err != nil {
		return nil, nil, err
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	return bounded, cancel, nil
}

// All fields remain private and are consumed inside runStage before DBOS sees
// an error. Only the existing encrypted log helpers may persist captured bytes.
type operationDeadline struct {
	phase        string
	code         string
	step         string
	retry        string
	logs         []byte
	logsTooLarge bool
}

func (*operationDeadline) Error() string { return "operation_timed_out" }

func (e *Engine) deadlineFailure(ctx context.Context, phase, code string, logs []byte, tooLarge bool) *operationDeadline {
	if e.config.Postgres == nil || ctx.Err() == nil {
		return nil
	}
	// The extra byte preserves the existing finite oversize branch for Apply.
	captured := bytes.Clone(logs[:min(len(logs), rolloutexec.PlanLogLimit+1)])
	return &operationDeadline{phase: phase, code: code, logs: captured, logsTooLarge: tooLarge}
}

func (e *Engine) applyDeadline(ctx context.Context, out rolloutexec.Outcome, executionErr error, prepareCode string) *operationDeadline {
	code := "apply_outcome_unknown"
	var refusal *Error
	if executionErr != nil && !out.Started && !errors.As(executionErr, &refusal) {
		code = prepareCode
	}
	return e.deadlineFailure(ctx, "apply", code, out.Logs, false)
}

func (e *Engine) recordOperationDeadline(ctx context.Context, id string, failure *operationDeadline) error {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var err error
	switch failure.phase {
	case "post-tasks":
		err = e.taskFailed(bounded, id, failure.step, rolloutexec.Tool{Phase: "post_apply", Retry: failure.retry}, "task_timed_out")
	case "plan":
		err = e.saveFailedPlanLogs(bounded, id, rolloutexec.Plan{Logs: failure.logs, LogsTooLarge: failure.logsTooLarge})
		if failure.logsTooLarge {
			failure.code = "plan_logs_too_large"
		}
	case "apply":
		err = e.saveExecutionLogs(bounded, id, failure.logs)
	default:
		return refused("execution_limits_refused")
	}
	if err == nil && failure.phase != "post-tasks" {
		phase := "failed"
		if failure.code == "apply_outcome_unknown" {
			phase = "outcome_unknown"
		}
		err = e.finish(bounded, id, phase, failure.code)
	}
	var refusal *Error
	if err != nil && !errors.As(err, &refusal) {
		e.storageFailed(bounded)
	}
	if interrupted := e.interrupted(); interrupted != nil {
		return interrupted
	}
	return err
}
