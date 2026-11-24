package rollout

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
)

// AcceptControl authenticates Native authority before replay or domain mutation.
// Cancellation promises committed state and wake, not a process kill.
func (e *Engine) AcceptControl(ctx context.Context, signed []byte) (signedcontrol.Receipt, error) {
	if e.config.Postgres == nil {
		return signedcontrol.Receipt{}, refused("control_scope_refused")
	}
	request, payload, err := signedcontrol.Decode(signed, e.config.PublicKey)
	if err != nil {
		return signedcontrol.Receipt{}, err
	}
	if err = e.checkAdmission(); err != nil {
		return signedcontrol.Receipt{}, err
	}
	tx, err := e.beginCommand(ctx, request.RolloutID)
	if err != nil {
		return signedcontrol.Receipt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	command, err := e.controlScope(ctx, tx, request)
	if err != nil {
		return signedcontrol.Receipt{}, err
	}
	previous, err := e.readControl(ctx, request.ID)
	if err != nil {
		return signedcontrol.Receipt{}, err
	}
	if previous != nil {
		if previous.Request != string(payload) || previous.RequestDigest != signedcontrol.Digest(payload) {
			return signedcontrol.Receipt{}, refused("control_input_conflict")
		}
		return *previous, nil
	}
	receipt, err := e.recordControl(ctx, tx, command, request, payload)
	if err != nil {
		return signedcontrol.Receipt{}, err
	}
	client, cancel := e.transactionClient(ctx)
	defer cancel()
	_, err = dbos.Enqueue[signedcontrol.Receipt](client, controlReceiptQueue, controlReceiptWorkflow, receipt,
		dbos.WithEnqueueWorkflowID(e.controlWorkflowID(request.ID)), dbos.WithEnqueueApplicationName(controlApplication),
		dbos.WithEnqueueWorkflowIDReusePolicy(dbos.WorkflowIDReusePolicyReject), dbos.WithEnqueueTransaction(tx))
	if err != nil {
		return signedcontrol.Receipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return signedcontrol.Receipt{}, err
	}
	return receipt, nil
}

func (e *Engine) controlScope(ctx context.Context, tx *sql.Tx, request signedcontrol.Request) (Command, error) {
	command, err := e.loadCommand(ctx, tx, request.RolloutID)
	if err != nil {
		return command, err
	}
	var executionDigest string
	if err = tx.QueryRowContext(ctx, `SELECT digest FROM rollout_commands WHERE id=$1`, request.RolloutID).Scan(&executionDigest); err != nil {
		return command, err
	}
	if command.NativeTarget == nil || request.OrgID != command.OrgID || request.EnvironmentID != command.EnvironmentID || request.Target != signedcontrol.Target(*command.NativeTarget) || request.ExecutionDigest != executionDigest || request.IssuedAt.Before(command.IssuedAt) || request.Action != "repair" && request.ExpiresAt.After(command.ExpiresAt) {
		return command, refused("control_scope_refused")
	}
	return command, nil
}

func (e *Engine) recordControl(ctx context.Context, tx *sql.Tx, command Command, request signedcontrol.Request, payload []byte) (signedcontrol.Receipt, error) {
	p, err := readProgress(ctx, tx, request.RolloutID)
	if err != nil {
		return signedcontrol.Receipt{}, err
	}
	if p.ID != request.RolloutID || p.EnvironmentID != request.EnvironmentID {
		return signedcontrol.Receipt{}, refused("control_scope_refused")
	}
	if _, err = tx.ExecContext(ctx, `SAVEPOINT native_control_action`); err != nil {
		return signedcontrol.Receipt{}, err
	}
	err = e.applyControl(ctx, tx, &p, command, request)
	outcome, code := "applied", ""
	if err != nil {
		var refusal *Error
		if !errors.As(err, &refusal) {
			return signedcontrol.Receipt{}, err
		}
		if _, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT native_control_action`); err != nil {
			return signedcontrol.Receipt{}, err
		}
		// Approval may mutate aliased progress and delete the gate before refusing.
		// Discard the entire action value and reload only after the SQL rollback.
		p = Progress{}
		p, err = readProgress(ctx, tx, request.RolloutID)
		if err != nil {
			return signedcontrol.Receipt{}, err
		}
		outcome, code = "refused", refusal.Code
	} else {
		if err = recordControlAudit(ctx, tx, request); err != nil {
			return signedcontrol.Receipt{}, err
		}
		if err = saveProgress(ctx, tx, &p); err != nil {
			return signedcontrol.Receipt{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT native_control_action`); err != nil {
		return signedcontrol.Receipt{}, err
	}
	return signedcontrol.Freeze(request, payload, outcome, code, p.Phase, p.Revision)
}

func (e *Engine) applyControl(ctx context.Context, tx *sql.Tx, p *Progress, command Command, request signedcontrol.Request) error {
	now := time.Now()
	if request.IssuedAt.After(now.Add(time.Minute)) || !request.ExpiresAt.After(now) || request.Action != "repair" && !validLifetime(command, now) {
		return refused("control_expired")
	}
	switch request.Action {
	case "approve", "deny":
		return e.recordApproval(ctx, tx, p, request.Subject, request.Action, request.ActorID)
	case "cancel":
		return e.recordCancellation(ctx, tx, p, request.RolloutID)
	case "repair":
		if p.Revision != request.Revision {
			return refused("repair_revision_conflict")
		}
		return e.recordPostRepair(ctx, tx, p, command, request)
	default:
		return refused("invalid_control")
	}
}

func recordControlAudit(ctx context.Context, tx *sql.Tx, request signedcontrol.Request) error {
	subject := request.Subject
	if request.Action == "repair" {
		subject = request.FailedStep
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO rollout_decisions(rollout,kind,subject,actor,created_at) VALUES($1,$2,$3,$4,$5)`, request.RolloutID, request.Action, subject, request.ActorID, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// Management reads must carry their deadline in the SDK context itself.
// The transaction context-value bridge is only for caller-owned SQL transactions.
func (e *Engine) managementClient(ctx context.Context) (dbos.Context, context.CancelFunc) {
	limit := 15 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, time.Until(deadline))
	}
	client, cancel := dbos.WithTimeout(e.durable, limit)
	stop := context.AfterFunc(ctx, cancel)
	if ctx.Err() != nil {
		cancel()
	}
	return client, func() {
		stop()
		cancel()
	}
}

func (e *Engine) readControl(ctx context.Context, id string) (*signedcontrol.Receipt, error) {
	client, cancel := e.managementClient(ctx)
	defer cancel()
	rows, err := dbos.ListWorkflows(client, dbos.WithFilterWorkflowIDs(e.controlWorkflowID(id)), dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	if len(rows) != 1 {
		return nil, refused("control_receipt_conflict")
	}
	receipt, _, err := e.decodeControlRow(rows[0])
	return &receipt, err
}

func (e *Engine) decodeControlRow(row dbos.WorkflowStatus) (signedcontrol.Receipt, signedcontrol.Request, error) {
	raw, ok := row.Input.(string)
	if !ok || row.Name != controlReceiptWorkflow || row.QueueName != controlReceiptQueue || row.ApplicationName != controlApplication {
		return signedcontrol.Receipt{}, signedcontrol.Request{}, refused("control_receipt_conflict")
	}
	receipt, request, _, err := signedcontrol.DecodeReceipt([]byte(raw))
	if err != nil {
		return receipt, request, err
	}
	if row.ID != e.controlWorkflowID(request.ID) || request.OrgID != e.config.OrgID || request.Target.RunnerID != e.config.RunnerID || request.Target.RunnerUUID != e.config.RunnerUUID {
		return signedcontrol.Receipt{}, request, refused("control_receipt_conflict")
	}
	return receipt, request, nil
}

// AcknowledgeControl accepts only the frozen result digest and also works while
// the receipt is ENQUEUED. The public notification is retained until receive.
func (e *Engine) AcknowledgeControl(ctx context.Context, id, resultDigest string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if e.config.Postgres == nil || !signedcontrol.UUID(id) {
		return refused("control_scope_refused")
	}
	receipt, err := e.readControl(ctx, id)
	if err != nil {
		return err
	}
	if receipt == nil || receipt.ResultDigest != resultDigest {
		return refused("control_ack_conflict")
	}
	request, err := signedcontrol.DecodeRequest([]byte(receipt.Request))
	if err != nil {
		return err
	}
	tx, err := e.beginCommand(ctx, request.RolloutID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	client, cancelClient := e.transactionClient(ctx)
	defer cancelClient()
	if err = dbos.Send(client, e.controlWorkflowID(id), resultDigest, "delivered", dbos.WithIdempotencyKey("delivered:"+resultDigest), dbos.WithSendTransaction(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

// ControlDelivery is one validated retained receipt, including attention status.
type ControlDelivery struct {
	Receipt   signedcontrol.Receipt
	Status    string
	CreatedAt time.Time
}

// ControlDeliveries returns one bounded page. The Agent owns the volatile offset
// and publication deadline; failed receipts are a separate attention sweep.
func (e *Engine) ControlDeliveries(ctx context.Context, offset int, failed bool) ([]ControlDelivery, error) {
	if e.config.Postgres == nil || offset < 0 {
		return nil, refused("control_scope_refused")
	}
	statuses := []dbos.WorkflowStatusType{dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued}
	if failed {
		statuses = []dbos.WorkflowStatusType{dbos.WorkflowStatusError, dbos.WorkflowStatusCancelled, dbos.WorkflowStatusMaxRecoveryAttemptsExceeded}
	}
	client, cancel := e.managementClient(ctx)
	defer cancel()
	rows, err := dbos.ListWorkflows(client, dbos.WithFilterApplicationName(controlApplication), dbos.WithFilterQueueName(controlReceiptQueue), dbos.WithFilterName(controlReceiptWorkflow), dbos.WithFilterWorkflowIDPrefix(e.controlWorkflowPrefix()), dbos.WithFilterStatus(statuses...), dbos.WithFilterLimit(100), dbos.WithFilterOffset(offset), dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	deliveries := make([]ControlDelivery, 0, len(rows))
	for _, row := range rows {
		receipt, _, err := e.decodeControlRow(row)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, ControlDelivery{Receipt: receipt, Status: string(row.Status), CreatedAt: row.CreatedAt})
	}
	return deliveries, nil
}

const controlApplication = "stellwerk-runner-rollouts"
const controlReceiptQueue = "site-control-receipts"
const controlReceiptWorkflow = "stellwerk.pipeline.control-receipt/v1"

func (e *Engine) controlWorkflowPrefix() string {
	return "native-control:" + e.config.OrgID + ":" + e.config.RunnerUUID + ":"
}
func (e *Engine) controlWorkflowID(id string) string { return e.controlWorkflowPrefix() + id }

func controlReceipt(ctx dbos.Context, input signedcontrol.Receipt) (signedcontrol.Receipt, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return signedcontrol.Receipt{}, err
	}
	if _, _, _, err = signedcontrol.DecodeReceipt(encoded); err != nil {
		return signedcontrol.Receipt{}, err
	}
	return waitControlAcknowledgement(ctx, input, 10*time.Minute)
}

func waitControlAcknowledgement(ctx dbos.Context, input signedcontrol.Receipt, timeout time.Duration) (signedcontrol.Receipt, error) {
	for {
		ack, err := dbos.Recv[string](ctx, "delivered", timeout)
		if errors.Is(err, dbos.ErrTimeout) {
			continue
		}
		if err != nil {
			return signedcontrol.Receipt{}, err
		}
		if ack == input.ResultDigest {
			return input, nil
		}
		if ack != "" {
			if err = dbos.SetEvent(ctx, "delivery_attention", "ack_digest_conflict"); err != nil {
				return signedcontrol.Receipt{}, err
			}
		}
	}
}
