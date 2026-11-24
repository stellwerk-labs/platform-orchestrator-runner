package rollout

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// Decide records a local exact-plan decision. The host must authenticate and
// authorize actor; it is an audit identity, not a permission token.
func (e *Engine) Decide(ctx context.Context, id, subject, decision, actor string) (Progress, error) {
	if !auditActor(actor) || (decision != "approve" && decision != "deny") {
		return Progress{}, refused("invalid_decision")
	}
	return e.action(ctx, id, actor, decision, subject, func(tx *sql.Tx, p *Progress) error { return e.recordApproval(ctx, tx, p, subject, decision, actor) })
}

func (e *Engine) recordApproval(ctx context.Context, tx *sql.Tx, p *Progress, subject, decision, actor string) error {
	id := p.ID
	if p.Phase != "awaiting_approval" || p.Subject != subject || subject == "" {
		return refused("approval_subject_conflict")
	}
	command, err := e.commandInTransaction(ctx, tx, id)
	if err != nil {
		return err
	}
	if err = e.validate(command); err != nil {
		return err
	}
	if decision == "approve" {
		p.Phase = "approved"
	} else {
		p.Phase = "denied"
		if _, err = tx.ExecContext(ctx, `DELETE FROM rollout_environments WHERE rollout=$1`, id); err != nil {
			return err
		}
	}
	_, evidence, err := e.costEvidence(ctx, tx, id)
	if err != nil {
		return err
	}
	if evidence != "" {
		if err = e.verifyCostSubject(ctx, tx, id, subject, evidence); err != nil {
			return err
		}
	}
	audit := ApprovalDecision{Source: "human", Decision: decision, ActorID: actor, PolicyID: command.Approval.PolicyID, PolicyVersion: command.Approval.PolicyVersion, DecidedAt: time.Now().UTC(), Reason: "human_review", Subject: subject, EvidenceDigest: evidence}
	if err = recordApprovalDecision(ctx, tx, p, audit); err != nil {
		return err
	}
	client, cancel := e.transactionClient(ctx)
	defer cancel()
	return dbos.Send(client, id, subject, "approval", dbos.WithIdempotencyKey("approval:"+subject), dbos.WithSendTransaction(tx))
}

func (e *Engine) Cancel(ctx context.Context, id, actor string) (Progress, error) {
	if !auditActor(actor) {
		return Progress{}, refused("invalid_actor")
	}
	p, err := e.action(ctx, id, actor, "cancel", "", func(tx *sql.Tx, p *Progress) error {
		return e.recordCancellation(ctx, tx, p, id)
	})
	if err == nil {
		_ = dbos.CancelWorkflow(e.durable, id)
	}
	return p, err
}

func (e *Engine) Repair(ctx context.Context, id, step, actor string) (Progress, error) {
	if !auditActor(actor) {
		return Progress{}, refused("invalid_actor")
	}
	return e.action(ctx, id, actor, "repair", step, func(tx *sql.Tx, p *Progress) error {
		return e.recordRepair(ctx, tx, p, id, step)
	})
}

func (e *Engine) recordCancellation(ctx context.Context, tx *sql.Tx, p *Progress, id string) error {
	// This transaction races the observer's phase transition, before any
	// subprocess starts. An active observer retains ownership until it exits.
	if p.Phase == "observing" {
		return refused("observation_in_progress")
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM rollout_effects WHERE rollout=$1 AND step='apply'`, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 || terminal(p.Phase) {
		return refused("effects_require_reconciliation")
	}
	p.Phase = "cancelled"
	if _, err := tx.ExecContext(ctx, `DELETE FROM rollout_environments WHERE rollout=$1`, id); err != nil {
		return err
	}
	if e.config.Postgres != nil {
		// Durable cancellation alone does not wake a blocked public Recv.
		// Wake the gate in this transaction; native terminal/claim checks win.
		client, cancel := e.transactionClient(ctx)
		defer cancel()
		return dbos.Send(client, id, "cancelled", "approval", dbos.WithIdempotencyKey("cancel:"+id), dbos.WithSendTransaction(tx))
	}
	return nil
}

func (e *Engine) recordRepair(ctx context.Context, tx *sql.Tx, p *Progress, id, step string) error {
	if p.Phase != "post_failed" || p.FailedStep != step {
		return refused("repair_refused")
	}
	command, err := e.commandInTransaction(ctx, tx, id)
	if err != nil {
		return err
	}
	if !e.repairablePostStep(command, step) {
		return refused("effects_require_reconciliation")
	}
	if err = e.validate(command); err != nil {
		return err
	}
	return e.recordRepairStep(ctx, tx, p, command, step)
}

func (e *Engine) recordRepairStep(ctx context.Context, tx *sql.Tx, p *Progress, command Command, step string) error {
	id := command.ID
	if _, err := tx.ExecContext(ctx, `DELETE FROM rollout_effects WHERE rollout=$1 AND step=$2 AND status!='done'`, id, "task:"+step); err != nil {
		return err
	}
	p.Phase, p.ErrorCode = "post_retrying", ""
	client, cancel := e.transactionClient(ctx)
	defer cancel()
	return dbos.Send(client, id, step, "repair", dbos.WithIdempotencyKey(fmt.Sprintf("repair:%s:%d", step, p.Revision)), dbos.WithSendTransaction(tx))
}

func (e *Engine) action(ctx context.Context, id, actor, kind, subject string, apply func(*sql.Tx, *Progress) error) (Progress, error) {
	if err := e.checkAdmission(); err != nil {
		return Progress{}, err
	}
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return Progress{}, err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := readProgress(ctx, tx, id)
	if err == nil {
		err = apply(tx, &p)
	}
	if err != nil {
		return Progress{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_decisions(rollout,kind,subject,actor,created_at) VALUES($1,$2,$3,$4,$5)`, id, kind, subject, actor, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return Progress{}, err
	}
	if err = saveProgress(ctx, tx, &p); err != nil {
		return Progress{}, err
	}
	return p, tx.Commit()
}
