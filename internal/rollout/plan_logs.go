package rollout

import (
	"context"
	"database/sql"
	"errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

type PlanLogs struct {
	Subject string
	Content []byte
}

// GetPlanLogs reads only captured logs; it never regenerates a plan or exports keys.
func (e *Engine) GetPlanLogs(ctx context.Context, id, subject string) (PlanLogs, error) {
	p, err := e.Get(ctx, id)
	if err != nil {
		var missing *Error
		if errors.As(err, &missing) && missing.Code == "not_found" {
			return PlanLogs{}, refused("plan_logs_unavailable")
		}
		return PlanLogs{}, err
	}
	if p.Subject != subject {
		return PlanLogs{}, refused("plan_logs_subject_changed")
	}
	var stored string
	var content []byte
	var tooLarge bool
	err = e.db.QueryRowContext(ctx, `SELECT subject,content,too_large FROM rollout_plan_logs WHERE rollout=$1`, id).Scan(&stored, &content, &tooLarge)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanLogs{}, refused("plan_logs_unavailable")
	}
	if err != nil {
		return PlanLogs{}, err
	}
	if stored != subject {
		return PlanLogs{}, refused("plan_logs_subject_changed")
	}
	if tooLarge {
		return PlanLogs{}, refused("plan_logs_too_large")
	}
	plain, err := e.decryptLogs(ctx, e.db, id, "plan", subject, content)
	if err != nil {
		var limit *Error
		if errors.As(err, &limit) && limit.Code == "artifact_too_large" {
			return PlanLogs{}, refused("plan_logs_too_large")
		}
		return PlanLogs{}, err
	}
	return PlanLogs{Subject: subject, Content: plain}, nil
}

func (e *Engine) insertPlanLogs(ctx context.Context, tx *sql.Tx, command Command, p *Progress, plan rolloutexec.Plan) ([]byte, error) {
	var content, attachment []byte
	var err error
	if command.NativeTarget != nil {
		authority, authorityErr := e.logAuthority(ctx, tx, command, *p, "plan", p.Subject)
		if authorityErr != nil {
			return nil, authorityErr
		}
		var descriptor nativelogs.Descriptor
		content, descriptor, attachment, err = e.prepareNativeLogs(authority, command.Request.EncryptedLogsRecipient, plan.Logs, plan.LogsTooLarge)
		p.LogCapture = &descriptor
	} else {
		content, err = e.encrypt(plan.Logs)
	}
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO rollout_plan_logs(rollout,subject,content,too_large) VALUES($1,$2,$3,$4)
 ON CONFLICT(rollout) DO UPDATE SET subject=excluded.subject,content=excluded.content,too_large=excluded.too_large
 WHERE rollout_plan_logs.subject=''`, command.ID, p.Subject, content, plan.LogsTooLarge)
	return attachment, err
}

func (e *Engine) saveFailedPlanLogs(ctx context.Context, id string, plan rolloutexec.Plan) error {
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	command, err := e.commandInTransaction(ctx, tx, id)
	if err != nil {
		return err
	}
	p, err := readProgress(ctx, tx, id)
	if err != nil {
		return err
	}
	p.Subject = ""
	attachment, err := e.insertPlanLogs(ctx, tx, command, &p, plan)
	if err != nil {
		return err
	}
	if command.NativeTarget != nil {
		if err = e.saveLogCapture(ctx, tx, &p, *p.LogCapture, attachment); err != nil {
			return err
		}
	}
	return tx.Commit()
}
