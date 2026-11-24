package rollout

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

const RecipeCapability = "pipeline.recipe.v1"

func (e *Engine) validateRecipe(command Command) error {
	if command.Recipe == nil {
		return nil
	}
	r := command.Recipe
	hash, err := hex.DecodeString(r.Digest)
	if command.Version != e.pipelineVersion() || !identifier.MatchString(r.ID) || r.Version < 1 || err != nil || len(hash) != 32 || !slices.Contains(command.RequiredCapabilities, RecipeCapability) {
		return refused("recipe_refused")
	}
	return nil
}

// A child may not drop a required check, approval rule, or post task from its
// source's signed closure. The source Plan executed only its read-only prefix.
func sameRecipe(child, source Command) bool {
	if child.Recipe == nil && source.Recipe == nil {
		return true
	}
	if child.Recipe == nil || source.Recipe == nil {
		return false
	}
	left, _ := json.Marshal([]any{child.Recipe, child.Steps, child.Approval, child.Cost})
	right, _ := json.Marshal([]any{source.Recipe, source.Steps, source.Approval, source.Cost})
	return bytes.Equal(left, right)
}

func (e *Engine) inheritSavedReview(ctx context.Context, tx *sql.Tx, command Command, p *Progress) error {
	material, err := e.savedSource(ctx, tx, command)
	if err != nil {
		return err
	}
	var commandDigest string
	if err = tx.QueryRowContext(ctx, `SELECT digest FROM rollout_commands WHERE id=$1`, command.ID).Scan(&commandDigest); err != nil {
		return err
	}
	review, err := e.decrypt(material.review)
	if err != nil {
		return err
	}
	logs, err := e.decryptLogs(ctx, tx, command.SourcePlan.SourcePlanID, "plan", material.subject, material.logs)
	if err != nil {
		return err
	}
	p.Subject = reviewSubject(commandDigest, material.Ciphertext, review, logs, material.CostDigest)
	p.PlanChanges, p.Changes = material.Progress.PlanChanges, material.Progress.PlanChanges
	cost, _, err := e.costEvidence(ctx, tx, command.SourcePlan.SourcePlanID)
	if err != nil {
		return err
	}
	p.Cost, p.Steps = cost, material.Progress.Steps
	if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_artifacts(rollout,plan,review,digest) VALUES($1,$2,$3,$4)`, command.ID, material.Ciphertext, material.review, material.Digest); err != nil {
		return err
	}
	if command.NativeTarget == nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_plan_logs(rollout,subject,content,too_large) VALUES($1,$2,$3,$4)`, command.ID, p.Subject, material.logs, material.tooLarge); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_cost_artifacts(rollout,content,digest) SELECT $1,content,digest FROM rollout_cost_artifacts WHERE rollout=$2`, command.ID, command.SourcePlan.SourcePlanID); err != nil {
		return err
	}
	if command.NativeTarget != nil {
		attachment, captureErr := e.insertPlanLogs(ctx, tx, command, p, rolloutexec.Plan{Logs: logs, LogsTooLarge: material.tooLarge})
		if captureErr != nil {
			return captureErr
		}
		return e.saveLogCapture(ctx, tx, p, *p.LogCapture, attachment)
	}
	return nil
}

// New published-recipe children get their own review decision over the inherited
// exact evidence. Old builtin children retain their original durable stage order.
func (e *Engine) executeRecipeSaved(ctx dbos.Context, id string) (reference, error) {
	command, err := e.command(ctx, id)
	if err != nil {
		return reference{ID: id}, err
	}

	if command.Approval.Mode == "" {
		if _, err = e.stage(ctx, id, "saved-ready", func(ctx context.Context, id string) error { return e.phase(ctx, id, "preparing") }); err != nil {
			return reference{ID: id}, err
		}
		return e.executeSavedPipeline(ctx, id)
	}
	result, err := e.stage(ctx, id, "approval-ready", e.approvalReady)
	if err != nil || terminal(result.Phase) {
		return result, err
	}
	if result.Phase != "approved" {
		if err = e.waitApproval(ctx, result); err != nil {
			return result, err
		}
	}
	return e.executeSavedPipeline(ctx, id)
}

func (e *Engine) executeSavedPipeline(ctx dbos.Context, id string) (reference, error) {
	result, err := e.stage(ctx, id, "saved-apply", e.savedApply)
	if err != nil || terminal(result.Phase) {
		return result, err
	}
	return e.executePostTasks(ctx, id)
}
