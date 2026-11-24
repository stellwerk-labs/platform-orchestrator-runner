package rollout

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"time"
)

func (e *Engine) approvalReady(ctx context.Context, id string) error {
	command, err := e.checkedCommand(ctx, id)
	if err != nil {
		return err
	}
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := readProgress(ctx, tx, id)
	if err != nil {
		return err
	}
	if terminal(p.Phase) || p.Subject == "" || p.plannedChanges() == nil {
		return refused("approval_material_unavailable")
	}
	if p.Phase == "approved" {
		return nil
	}
	p.Phase = "awaiting_approval"
	automatic, reason, evidence, err := e.automaticApproval(ctx, tx, command, &p)
	if err != nil {
		return err
	}
	if automatic {
		p.Phase = "approved"
		decision := ApprovalDecision{Source: "policy", Decision: "approve", PolicyID: command.Approval.PolicyID,
			PolicyVersion: command.Approval.PolicyVersion, DecidedAt: time.Now().UTC(), Reason: reason, Subject: p.Subject, EvidenceDigest: evidence}
		if err = recordApprovalDecision(ctx, tx, &p, decision); err != nil {
			return err
		}
	}
	if err = saveProgress(ctx, tx, &p); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) automaticApproval(ctx context.Context, tx *sql.Tx, command Command, p *Progress) (bool, string, string, error) {
	switch command.Approval.Mode {
	case "policy":
		counts := p.plannedChanges()
		limits := command.Approval
		if counts.Added > limits.MaxAdd || counts.Change > limits.MaxChange || counts.Remove > limits.MaxDestroy || counts.Import > 0 {
			return false, "", "", refused("approval_policy_denied")
		}
		return true, "resource_limits_satisfied", "", nil
	case "cost_non_increasing":
		cost, evidence, err := e.costEvidence(ctx, tx, command.ID)
		if err != nil {
			return false, "", "", err
		}
		if cost == nil {
			return false, "", "", refused("approval_material_unavailable")
		}
		if err = e.verifyCostSubject(ctx, tx, command.ID, p.Subject, evidence); err != nil {
			return false, "", "", err
		}
		p.Cost = cost
		return cost.NonIncreasing(), "cost_non_increasing", evidence, nil
	default:
		return false, "", "", nil
	}
}

func recordApprovalDecision(ctx context.Context, tx *sql.Tx, p *Progress, decision ApprovalDecision) error {
	body, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_approval_audit(rollout,decision) VALUES($1,$2)`, p.ID, body); err != nil {
		return err
	}
	p.ApprovalDecision = &decision
	return nil
}

// Before typed outcomes, the reviewed stage stored its immutable Plan counts in Changes.
func (p Progress) plannedChanges() *runner.IaCChanges {
	if p.PlanChanges != nil {
		return p.PlanChanges
	}
	return p.Changes
}
