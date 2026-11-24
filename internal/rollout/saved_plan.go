package rollout

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

type savedPlanMaterial struct {
	Request    rolloutexec.Request
	Ciphertext []byte
	Digest     string
	CostDigest string
	Progress   Progress
	subject    string
	review     []byte
	logs       []byte
	tooLarge   bool
}

func validatePlanAuthority(command Command) error {
	if command.PlanBase != nil && (command.Strategy != "plan" || !identifier.MatchString(command.PlanBase.BaseDeploymentID)) {
		return refused("strategy_refused")
	}
	if command.Strategy != "apply_saved" {
		if command.SourcePlan != nil {
			return refused("strategy_refused")
		}
		return nil
	}
	source := command.SourcePlan
	if source == nil || !identifier.MatchString(source.SourcePlanID) || source.SourcePlanID == command.ID || !identifier.MatchString(source.BaseDeploymentID) || source.Subject == "" || command.Recipe == nil && len(command.Steps) != 0 {
		return refused("saved_plan_refused")
	}
	return nil
}

func (e *Engine) savedSource(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, command Command) (savedPlanMaterial, error) {
	var material savedPlanMaterial
	var payload, progress []byte
	var commandDigest, version, environment, strategy string
	err := query.QueryRowContext(ctx, `SELECT c.payload,c.digest,c.progress,a.plan,a.digest,a.review,l.subject,l.content,l.too_large,c.version,c.environment,c.strategy
 FROM rollout_commands c JOIN rollout_artifacts a ON a.rollout=c.id JOIN rollout_plan_logs l ON l.rollout=c.id WHERE c.id=$1`, command.SourcePlan.SourcePlanID).
		Scan(&payload, &commandDigest, &progress, &material.Ciphertext, &material.Digest, &material.review, &material.subject, &material.logs, &material.tooLarge, &version, &environment, &strategy)
	if errors.Is(err, sql.ErrNoRows) {
		return material, refused("saved_plan_unavailable")
	}
	if err != nil {
		return material, err
	}
	var state Progress
	source, err := e.decryptStoredCommand(command.SourcePlan.SourcePlanID, commandDigest, payload, version, environment, strategy)
	if err != nil || json.Unmarshal(progress, &state) != nil {
		return material, refused("saved_plan_unavailable")
	}
	if err = e.matchSavedSource(command, source, state); err != nil {
		return material, err
	}
	if material.subject != state.Subject {
		return material, refused("saved_plan_subject_conflict")
	}
	_, material.CostDigest, err = e.costEvidence(ctx, query, source.ID)
	if err != nil {
		return material, err
	}
	material.Progress = state
	if err = e.verifySavedArtifact(ctx, query, source, material, commandDigest); err != nil {
		return material, err
	}
	if err = requireUnusedSource(ctx, query, command); err != nil {
		return material, err
	}
	if err = requireCurrentSource(ctx, query, command); err != nil {
		return material, err
	}
	material.Request = source.Request
	return material, nil
}

func (e *Engine) matchSavedSource(command, source Command, progress Progress) error {
	if !completedCapturedPlan(source, progress) || source.ID != command.SourcePlan.SourcePlanID {
		return refused("saved_plan_unavailable")
	}
	if progress.Subject != command.SourcePlan.Subject {
		return refused("saved_plan_subject_conflict")
	}
	if e.scope(source) != nil || source.Version != command.Version || source.RunnerUUID != command.RunnerUUID || e.validateStrategy(source) != nil || source.EnvironmentID != command.EnvironmentID || source.PlanBase.BaseDeploymentID != command.SourcePlan.BaseDeploymentID {
		return refused("saved_plan_scope_conflict")
	}
	if !sameNativeTarget(command.NativeTarget, source.NativeTarget) || !samePlanInputs(command, source) || !sameRecipe(command, source) {
		return refused("saved_plan_material_conflict")
	}
	if e.executor.Validate(source.Request, source.Steps) != nil {
		return refused("saved_plan_material_conflict")
	}
	return nil
}

func samePlanInputs(command, source Command) bool {
	return source.Request.Backend == command.Request.Backend && source.Request.Tool == command.Request.Tool && source.ToolsDigest == command.ToolsDigest &&
		source.Request.MetadataKey == command.Request.MetadataKey && bytes.Equal(source.Request.Manifest, command.Request.Manifest) && source.Request.BundleSHA256 == command.Request.BundleSHA256
}

func completedCapturedPlan(source Command, progress Progress) bool {
	return (source.Version == PipelineVersion || source.Version == NativePipelineVersion) && source.Strategy == "plan" && source.PlanBase != nil && progress.Phase == "completed" && !progress.InfrastructureApplied
}

func (e *Engine) verifySavedArtifact(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, source Command, material savedPlanMaterial, commandDigest string) error {
	if material.tooLarge {
		return refused("saved_plan_unavailable")
	}
	review, err := e.decrypt(material.review)
	if err != nil {
		return refused("saved_plan_unavailable")
	}
	logs, err := e.decryptLogs(ctx, query, source.ID, "plan", material.subject, material.logs)
	if err != nil || !json.Valid(review) {
		return refused("saved_plan_unavailable")
	}
	expected := reviewSubject(commandDigest, material.Ciphertext, review, logs, material.CostDigest)
	if material.Digest != digest(material.Ciphertext) || expected != material.subject {
		return refused("saved_plan_subject_conflict")
	}
	return nil
}

func requireUnusedSource(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, command Command) error {
	var usedBy string
	err := query.QueryRowContext(ctx, `SELECT rollout FROM rollout_plan_uses WHERE source_plan=$1`, command.SourcePlan.SourcePlanID).Scan(&usedBy)
	if err == nil && usedBy != command.ID {
		return refused("saved_plan_already_used")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func requireCurrentSource(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, command Command) error {
	var initial int64
	err := query.QueryRowContext(ctx, `SELECT id FROM rollout_events WHERE rollout=$1 AND revision=1`, command.SourcePlan.SourcePlanID).Scan(&initial)
	if errors.Is(err, sql.ErrNoRows) {
		return refused("saved_plan_unavailable")
	}
	if err != nil {
		return err
	}
	var newer bool
	err = query.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rollout_events e JOIN rollout_commands c ON c.id=e.rollout
 WHERE e.revision=1 AND e.id>$1 AND c.id<>$2 AND c.version=$3
 AND c.environment=$4 AND c.strategy<>'plan')`, initial, command.ID, command.Version, command.EnvironmentID).Scan(&newer)
	if err != nil {
		return err
	}
	if newer {
		return refused("saved_plan_superseded")
	}
	return nil
}

func consumeSourcePlan(ctx context.Context, tx *sql.Tx, command Command) error {
	if command.SourcePlan == nil {
		return nil
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO rollout_plan_uses(source_plan,rollout) VALUES($1,$2) ON CONFLICT(source_plan) DO NOTHING`, command.SourcePlan.SourcePlanID, command.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err == nil && changed == 0 {
		return refused("saved_plan_already_used")
	}
	return err
}

func (e *Engine) savedApply(ctx context.Context, id string) error {
	command, err := e.checkedCommand(ctx, id)
	if err != nil {
		return err
	}
	var exists int
	if err = e.db.QueryRowContext(ctx, `SELECT count(*) FROM rollout_pipeline_results WHERE rollout=$1`, id).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	material, err := e.savedSource(ctx, e.db, command)
	if err != nil {
		return err
	}
	outcome, err := e.executor.ApplySavedFrom(ctx, command.Request, material.Request, material.Ciphertext, material.Digest, func() error {
		if e.config.Postgres != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		status, claimErr := e.claim(ctx, id, "apply", true)
		if claimErr != nil {
			return claimErr
		}
		if status != "new" {
			return refused("apply_outcome_unknown")
		}
		return nil
	})
	if failure := e.applyDeadline(ctx, outcome, err, "saved_plan_unavailable"); failure != nil {
		return failure
	}
	if err != nil {
		var refusal *Error
		if logErr := e.saveExecutionLogs(ctx, id, outcome.Logs); logErr != nil {
			if e.config.Postgres != nil && (outcome.Started || errors.As(err, &refusal)) {
				return errors.Join(refused("apply_outcome_unknown"), refused("storage_unavailable"), logErr)
			}
			return refused("storage_unavailable")
		}
		if outcome.Started || errors.As(err, &refusal) {
			return refused("apply_outcome_unknown")
		}
		return refused("saved_plan_unavailable")
	}
	return e.savePipelineOutcome(ctx, id, outcome)
}

func sameNativeTarget(a, b *NativeExecutionTarget) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
