package rollout

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
)

func (e *Engine) validateCost(command Command) error {
	if command.Cost != nil && command.Cost.PolicyRevision < 0 {
		return refused("invalid_cost_policy")
	}
	enabled := command.Cost != nil && command.Cost.Enabled
	if command.Approval.Mode == "cost_non_increasing" && !enabled {
		return refused("approval_policy_refused")
	}
	if !enabled {
		return nil
	}
	if command.Version != e.pipelineVersion() || !slices.Contains([]string{"reviewed", "plan", "apply_saved"}, command.Strategy) || e.executor.Cost == nil || !slices.Contains(command.RequiredCapabilities, rolloutexec.CostCapability) || !slices.Contains(command.RequiredCapabilities, e.executor.PricingCapability()) {
		return refused("cost_capability_refused")
	}
	return nil
}

func reviewSubject(commandDigest string, plan, review, logs []byte, costDigest string) string {
	material := commandDigest + ":" + digest(plan) + ":" + digest(review) + ":" + digest(logs)
	// Preserve the exact subject algorithm for historical commands without cost.
	if costDigest != "" {
		material += ":cost/v1:" + costDigest
	}
	return digest([]byte(material))
}

func (e *Engine) insertCost(ctx context.Context, tx *sql.Tx, id string, cost *runner.CostComparison) (string, error) {
	if cost == nil {
		return "", nil
	}
	body, err := json.Marshal(cost)
	if err != nil || len(body) > 8192 {
		return "", refused("invalid_cost_evidence")
	}
	encrypted, err := e.encrypt(body)
	if err != nil {
		return "", err
	}
	hash := digest(body)
	_, err = tx.ExecContext(ctx, `INSERT INTO rollout_cost_artifacts(rollout,content,digest) VALUES($1,$2,$3)`, id, encrypted, hash)
	return hash, err
}

func (e *Engine) costEvidence(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (*runner.CostComparison, string, error) {
	var content []byte
	var expected string
	err := query.QueryRowContext(ctx, `SELECT content,digest FROM rollout_cost_artifacts WHERE rollout=$1`, id).Scan(&content, &expected)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	content, err = e.decrypt(content)
	if err != nil || digest(content) != expected {
		return nil, "", refused("invalid_cost_evidence")
	}
	var cost runner.CostComparison
	if json.Unmarshal(content, &cost) != nil {
		return nil, "", refused("invalid_cost_evidence")
	}
	return &cost, expected, nil
}

func (e *Engine) verifyCostSubject(ctx context.Context, tx *sql.Tx, id, subject, costDigest string) error {
	var commandDigest string
	var plan, review, logs []byte
	err := tx.QueryRowContext(ctx, `SELECT c.digest,a.plan,a.review,l.content FROM rollout_commands c
 JOIN rollout_artifacts a ON a.rollout=c.id JOIN rollout_plan_logs l ON l.rollout=c.id WHERE c.id=$1`, id).Scan(&commandDigest, &plan, &review, &logs)
	if err != nil {
		return err
	}
	review, err = e.decrypt(review)
	if err != nil {
		return refused("invalid_cost_evidence")
	}
	logs, err = e.decryptLogs(ctx, tx, id, "plan", subject, logs)
	if err != nil || reviewSubject(commandDigest, plan, review, logs, costDigest) != subject {
		return refused("invalid_cost_evidence")
	}
	return nil
}
