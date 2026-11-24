package rollout

import (
	"context"
	"database/sql"
	"errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

func (e *Engine) insertExecutionLogs(ctx context.Context, tx *sql.Tx, command Command, p *Progress, logs []byte) ([]byte, error) {
	tooLarge := len(logs) > rolloutexec.PlanLogLimit
	if tooLarge {
		logs = nil
	}
	var encrypted, attachment []byte
	var err error
	if command.NativeTarget != nil {
		authority, authorityErr := e.logAuthority(ctx, tx, command, *p, "apply", p.Subject)
		if authorityErr != nil {
			return nil, authorityErr
		}
		var descriptor nativelogs.Descriptor
		encrypted, descriptor, attachment, err = e.prepareNativeLogs(authority, command.Request.EncryptedLogsRecipient, logs, tooLarge)
		p.LogCapture = &descriptor
	} else {
		encrypted, err = e.encrypt(logs)
	}
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO rollout_execution_logs(rollout,content,too_large) VALUES($1,$2,$3)
 ON CONFLICT(rollout) DO UPDATE SET content=excluded.content,too_large=excluded.too_large`, command.ID, encrypted, tooLarge)
	return attachment, err
}

func (e *Engine) saveExecutionLogs(ctx context.Context, id string, logs []byte) error {
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
	attachment, err := e.insertExecutionLogs(ctx, tx, command, &p, logs)
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

func (e *Engine) GetExecutionLogs(ctx context.Context, id, subject string) (PlanLogs, error) {
	p, err := e.Get(ctx, id)
	if err != nil {
		return PlanLogs{}, err
	}
	if subject != "" {
		return PlanLogs{}, refused("plan_logs_subject_changed")
	}
	var content []byte
	var tooLarge bool
	err = e.db.QueryRowContext(ctx, `SELECT content,too_large FROM rollout_execution_logs WHERE rollout=$1`, id).Scan(&content, &tooLarge)
	if errors.Is(err, sql.ErrNoRows) {
		command, loadErr := e.command(ctx, id)
		if loadErr != nil {
			return PlanLogs{}, loadErr
		}
		if command.Version != e.pipelineVersion() || command.Strategy != "plan" {
			return PlanLogs{}, refused("plan_logs_unavailable")
		}
		logs, loadErr := e.GetPlanLogs(ctx, id, p.Subject)
		logs.Subject = ""
		return logs, loadErr
	}
	if err != nil {
		return PlanLogs{}, err
	}
	if tooLarge {
		return PlanLogs{}, refused("plan_logs_too_large")
	}
	plain, err := e.decryptLogs(ctx, e.db, id, "apply", "", content)
	return PlanLogs{Subject: subject, Content: plain}, err
}
