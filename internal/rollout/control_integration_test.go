//go:build integration

package rollout

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func nativeControlFixture(t *testing.T) (*fixture, signedcontrol.Request, Progress, string, *sql.DB) {
	t.Helper()
	f, trace, admin := postgresFixture(t, "reviewed")
	restrictControlRuntime(t, f, admin)
	f.command.ID, f.command.EnvironmentID = uuid.NewString(), uuid.NewString()
	f.command.Request.ID, f.command.Request.EnvironmentID = f.command.ID, f.command.EnvironmentID
	execution := f.signed(t, f.command)
	_, err := f.engine.Submit(t.Context(), execution)
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	request := signedcontrol.Request{Version: signedcontrol.Version, ID: uuid.NewString(), RolloutID: f.command.ID, OrgID: f.command.OrgID, EnvironmentID: f.command.EnvironmentID, Target: signedcontrol.Target(*f.command.NativeTarget), ExecutionDigest: digest(execution.Payload), ActorID: uuid.NewString(), Action: "deny", Subject: p.Subject, IssuedAt: time.Now().UTC(), ExpiresAt: f.command.ExpiresAt}
	return f, request, p, trace, admin
}

func signNativeControl(t *testing.T, f *fixture, request signedcontrol.Request) []byte {
	t.Helper()
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	encoded, err := json.Marshal(signedcontrol.Signed{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(f.key, payload))})
	require.NoError(t, err)
	return encoded
}

func receiptResult(t *testing.T, receipt signedcontrol.Receipt) signedcontrol.Result {
	t.Helper()
	var result signedcontrol.Result
	require.NoError(t, json.Unmarshal([]byte(receipt.Result), &result))
	return result
}

func TestPostgresNativeControlDenyReplayAndChangedIntent(t *testing.T) {
	f, request, _, trace, _ := nativeControlFixture(t)
	signed := signNativeControl(t, f, request)
	receipt, err := f.engine.AcceptControl(t.Context(), signed)
	require.NoError(t, err)
	result := receiptResult(t, receipt)
	require.Equal(t, "applied", result.Outcome)
	require.Equal(t, "denied", result.Phase)
	pending, err := f.engine.AcceptControl(t.Context(), signed)
	require.NoError(t, err)
	require.Equal(t, receipt, pending)
	changed := request
	changed.ActorID = uuid.NewString()
	_, err = f.engine.AcceptControl(t.Context(), signNativeControl(t, f, changed))
	requireCode(t, err, "control_input_conflict")
	forged := bytes.Replace(signed, []byte(request.ActorID), []byte(uuid.NewString()), 1)
	_, err = f.engine.AcceptControl(t.Context(), forged)
	require.ErrorIs(t, err, signedcontrol.ErrSignature)
	require.NoError(t, f.engine.AcknowledgeControl(t.Context(), request.ID, receipt.ResultDigest))
	require.Eventually(t, func() bool {
		rows, err := dbos.ListWorkflows(f.engine.durable, dbos.WithFilterWorkflowIDs(f.engine.controlWorkflowID(request.ID)))
		return err == nil && len(rows) == 1 && rows[0].Status == dbos.WorkflowStatusSuccess
	}, 10*time.Second, 20*time.Millisecond)
	reopenPostgres(t, f)
	replay, err := f.engine.AcceptControl(t.Context(), signed)
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
	request.ActorID = uuid.NewString()
	_, err = f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	requireCode(t, err, "control_input_conflict")
	var decisions int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_decisions WHERE rollout=$1 AND kind='deny'`, request.RolloutID).Scan(&decisions))
	require.Equal(t, 1, decisions)
	require.Zero(t, operationCount(t, trace, "apply"))
}

func TestPostgresNativeControlRefusalRestoresDeletedGate(t *testing.T) {
	f, request, before, trace, _ := nativeControlFixture(t)
	// Actual recordApproval deletes the gate and mutates p before reading this.
	_, err := f.engine.db.Exec(`INSERT INTO rollout_cost_artifacts(rollout,content,digest) VALUES($1,$2,$3)`, request.RolloutID, []byte("invalid encrypted evidence"), strings.Repeat("a", 64))
	require.NoError(t, err)
	receipt, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	result := receiptResult(t, receipt)
	require.Equal(t, "refused", result.Outcome)
	require.Equal(t, "invalid_cost_evidence", result.Code)
	after, err := f.engine.Get(t.Context(), request.RolloutID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	var gates, decisions int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_environments WHERE rollout=$1`, request.RolloutID).Scan(&gates))
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_decisions WHERE rollout=$1`, request.RolloutID).Scan(&decisions))
	require.Equal(t, 1, gates)
	require.Zero(t, decisions)
	require.Zero(t, operationCount(t, trace, "apply"))
	replay, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
}

func TestPostgresNativeControlSQLFailureRollsBackActionAndReceipt(t *testing.T) {
	f, request, before, _, admin := nativeControlFixture(t)
	// A genuine PostgreSQL constraint failure is transient, never a coded refusal.
	_, err := admin.Exec(`ALTER TABLE ` + pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema) + `.rollout_decisions ADD CONSTRAINT reject_native_control CHECK(kind<>'deny')`)
	require.NoError(t, err)
	_, err = f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.Error(t, err)
	after, err := f.engine.Get(t.Context(), request.RolloutID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	receipt, err := f.engine.readControl(t.Context(), request.ID)
	require.NoError(t, err)
	require.Nil(t, receipt)
	var gates int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_environments WHERE rollout=$1`, request.RolloutID).Scan(&gates))
	require.Equal(t, 1, gates)
}

func TestPostgresNativeControlCancelWakesWithoutSDKCancellation(t *testing.T) {
	f, request, _, trace, _ := nativeControlFixture(t)
	request.Action, request.Subject = "cancel", ""
	receipt, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	require.Equal(t, "cancelled", receiptResult(t, receipt).Phase)
	// AcceptControl does not call CancelWorkflow. The real execute workflow must
	// leave its approval receive through the committed wake and terminal check.
	require.Eventually(t, func() bool {
		rows, err := dbos.ListWorkflows(f.engine.durable, dbos.WithFilterWorkflowIDs(request.RolloutID))
		return err == nil && len(rows) == 1 && rows[0].Status == dbos.WorkflowStatusSuccess
	}, 10*time.Second, 20*time.Millisecond)
	require.Zero(t, operationCount(t, trace, "apply"))
}

func TestPostgresNativeControlScopeAndRepairRevisionRefuseBeforeAction(t *testing.T) {
	f, request, before, _, _ := nativeControlFixture(t)
	changed := request
	changed.Target.StateConfigurationDigest = strings.Repeat("e", 64)
	_, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, changed))
	requireCode(t, err, "control_scope_refused")
	changed = request
	changed.ExecutionDigest = strings.Repeat("e", 64)
	_, err = f.engine.AcceptControl(t.Context(), signNativeControl(t, f, changed))
	requireCode(t, err, "control_scope_refused")
	missing, err := f.engine.readControl(t.Context(), request.ID)
	require.NoError(t, err)
	require.Nil(t, missing)
	request.Action, request.Subject, request.FailedStep, request.Revision = "repair", "", "post-task", before.Revision+1
	receipt, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	require.Equal(t, "repair_revision_conflict", receiptResult(t, receipt).Code)
	after, err := f.engine.Get(t.Context(), request.RolloutID)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestPostgresNativeControlExpiredReplayPrecedesFreshExpiry(t *testing.T) {
	f, request, _, _, _ := nativeControlFixture(t)
	request.ExpiresAt = time.Now().Add(2 * time.Second).UTC()
	signed := signNativeControl(t, f, request)
	receipt, err := f.engine.AcceptControl(t.Context(), signed)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return time.Now().After(request.ExpiresAt) }, 3*time.Second, 20*time.Millisecond)
	replay, err := f.engine.AcceptControl(t.Context(), signed)
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
	request.ID = uuid.NewString()
	fresh, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	require.Equal(t, "control_expired", receiptResult(t, fresh).Code)
}

func TestPostgresNativeControlApproveRunsExactReviewedPlan(t *testing.T) {
	f, request, _, trace, _ := nativeControlFixture(t)
	request.Action = "approve"
	receipt, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	require.Equal(t, "approved", receiptResult(t, receipt).Phase)
	waitPhase(t, f.engine, request.RolloutID, "completed")
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

func TestPostgresNativeControlRepairRetainsOneApply(t *testing.T) {
	_, toolErr := exec.LookPath("tofu")
	require.NoError(t, toolErr)
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	marker := filepath.Join(t.TempDir(), "post-receipt")
	addTask(t, f, "post_apply", "receipt", fmt.Sprintf(`import json,sys,pathlib
r=json.load(sys.stdin)
p=pathlib.Path(%q)
if not p.exists():
 p.write_text(r['effect_id'])
 sys.exit(1)
assert p.read_text()==r['effect_id']
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, marker))
	admin := openPostgresFixture(t, f)
	restrictControlRuntime(t, f, admin)
	f.command.ID, f.command.EnvironmentID = uuid.NewString(), uuid.NewString()
	f.command.Request.ID, f.command.Request.EnvironmentID = f.command.ID, f.command.EnvironmentID
	execution := f.signed(t, f.command)
	_, err := f.engine.Submit(t.Context(), execution)
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "post_failed")
	request := signedcontrol.Request{Version: signedcontrol.Version, ID: uuid.NewString(), RolloutID: f.command.ID, OrgID: f.command.OrgID, EnvironmentID: f.command.EnvironmentID, Target: signedcontrol.Target(*f.command.NativeTarget), ExecutionDigest: digest(execution.Payload), ActorID: uuid.NewString(), Action: "repair", FailedStep: p.FailedStep, Revision: p.Revision, IssuedAt: time.Now().UTC(), ExpiresAt: f.command.ExpiresAt}
	receipt, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	require.Equal(t, "post_retrying", receiptResult(t, receipt).Phase)
	waitPhase(t, f.engine, f.command.ID, "completed")
	replay, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

func TestPostgresNativeControlReceiptTimeoutWrongAckAndEnqueuedAck(t *testing.T) {
	f, request, _, _, _ := nativeControlFixture(t)
	// Finish the domain workflow before replacing only the receipt runtime.
	// Shutting down active Engine work is an authority failure, not a healthy restart.
	ended, err := f.engine.Decide(t.Context(), request.RolloutID, request.Subject, "deny", request.ActorID)
	require.NoError(t, err)
	require.Equal(t, "denied", ended.Phase)
	require.Eventually(t, func() bool {
		rows, err := dbos.ListWorkflows(f.engine.durable, dbos.WithFilterWorkflowIDs(request.RolloutID))
		return err == nil && len(rows) == 1 && rows[0].Status == dbos.WorkflowStatusSuccess
	}, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, dbos.Shutdown(f.engine.durable, time.Second))
	durable, err := dbos.NewContext(context.Background(), dbos.Config{AppName: controlApplication, DatabaseURL: f.engine.postgresDSN, DatabaseSchema: f.config.Postgres.SystemSchema, ApplicationVersion: f.engine.runtimeVersion(), ExecutorID: "control-lifecycle:" + uuid.NewString(), SkipMigrations: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	require.NoError(t, err)
	f.engine.durable = durable
	dbos.ListenQueues(durable, rolloutQueue, controlReceiptQueue)
	dbos.RegisterWorkflow(durable, f.engine.execute, dbos.WithWorkflowName(f.engine.runtimeVersion()))
	// Exercise the production receive loop against real DBOS with a short wait.
	// The registered production entry retains its fixed 10-minute wait.
	dbos.RegisterWorkflow(durable, func(ctx dbos.Context, input signedcontrol.Receipt) (signedcontrol.Receipt, error) {
		return waitControlAcknowledgement(ctx, input, 20*time.Millisecond)
	}, dbos.WithWorkflowName(controlReceiptWorkflow))
	require.NoError(t, dbos.Launch(durable))
	request.Subject = strings.Repeat("f", 64)
	first, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		rows, err := dbos.ListWorkflows(durable, dbos.WithFilterWorkflowIDs(f.engine.controlWorkflowID(request.ID)))
		return err == nil && len(rows) == 1 && rows[0].Status == dbos.WorkflowStatusPending
	}, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, dbos.Send(durable, f.engine.controlWorkflowID(request.ID), "", "delivered"))
	require.NoError(t, dbos.Send(durable, f.engine.controlWorkflowID(request.ID), "wrong-digest", "delivered"))
	attention, err := dbos.GetEvent[string](durable, f.engine.controlWorkflowID(request.ID), "delivery_attention", 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "ack_digest_conflict", attention)
	require.Never(t, func() bool {
		rows, err := dbos.ListWorkflows(durable, dbos.WithFilterWorkflowIDs(f.engine.controlWorkflowID(request.ID)))
		return err != nil || len(rows) != 1 || rows[0].Status != dbos.WorkflowStatusPending
	}, 120*time.Millisecond, 20*time.Millisecond)
	secondRequest := request
	secondRequest.ID = uuid.NewString()
	second, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, secondRequest))
	require.NoError(t, err)
	deliveries, err := f.engine.ControlDeliveries(t.Context(), 0, false)
	require.NoError(t, err)
	foundEnqueued := false
	for _, delivery := range deliveries {
		if delivery.Receipt == second {
			foundEnqueued = delivery.Status == string(dbos.WorkflowStatusEnqueued)
		}
	}
	require.True(t, foundEnqueued, "single receipt worker is occupied by the first receipt")
	require.NoError(t, f.engine.AcknowledgeControl(t.Context(), secondRequest.ID, second.ResultDigest))
	require.NoError(t, f.engine.AcknowledgeControl(t.Context(), request.ID, first.ResultDigest))
	require.Eventually(t, func() bool {
		rows, err := dbos.ListWorkflows(durable, dbos.WithFilterWorkflowIDs(f.engine.controlWorkflowID(request.ID), f.engine.controlWorkflowID(secondRequest.ID)))
		return err == nil && len(rows) == 2 && rows[0].Status == dbos.WorkflowStatusSuccess && rows[1].Status == dbos.WorkflowStatusSuccess
	}, 10*time.Second, 20*time.Millisecond)
}

func restrictControlRuntime(t *testing.T, f *fixture, admin *sql.DB) {
	t.Helper()
	closeFixture(t, f.engine)
	role := "native_control_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	password := uuid.NewString()
	_, err := admin.Exec(`CREATE ROLE ` + pq.QuoteIdentifier(role) + ` LOGIN PASSWORD ` + pq.QuoteLiteral(password) + ` NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT`)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeFixture(t, f.engine)
		_, err := admin.Exec(`DROP OWNED BY ` + pq.QuoteIdentifier(role) + `; DROP ROLE ` + pq.QuoteIdentifier(role))
		require.NoError(t, err)
	})
	for _, schema := range []string{f.config.Postgres.ApplicationSchema, f.config.Postgres.SystemSchema} {
		_, err = admin.Exec(`GRANT USAGE ON SCHEMA ` + pq.QuoteIdentifier(schema) + ` TO ` + pq.QuoteIdentifier(role) + `;
GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA ` + pq.QuoteIdentifier(schema) + ` TO ` + pq.QuoteIdentifier(role) + `;
GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA ` + pq.QuoteIdentifier(schema) + ` TO ` + pq.QuoteIdentifier(role) + `;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA ` + pq.QuoteIdentifier(schema) + ` TO ` + pq.QuoteIdentifier(role))
		require.NoError(t, err)
	}
	rawDSN, err := readStorageSecret(f.config.Postgres.DSNFile, 8192)
	require.NoError(t, err)
	dsn := string(rawDSN)
	f.config.Postgres.DSNFile = filepath.Join(t.TempDir(), "runtime-dsn")
	require.NoError(t, os.WriteFile(f.config.Postgres.DSNFile, []byte(dsn+" user="+role+" password="+password), 0600))
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	tx, err := f.engine.beginCommand(t.Context())
	require.NoError(t, err)
	var actual string
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT current_user`).Scan(&actual))
	require.Equal(t, role, actual)
	require.NoError(t, tx.Rollback())
	// Privilege refusal uses an independent connection: the real worker tracer
	// intentionally treats any unexpected SQL error as a fatal storage failure.
	limited, err := sql.Open("postgres", dsn+" user="+role+" password="+password)
	require.NoError(t, err)
	defer func() { require.NoError(t, limited.Close()) }()
	for _, schema := range []string{f.config.Postgres.ApplicationSchema, f.config.Postgres.SystemSchema} {
		_, err = limited.ExecContext(t.Context(), `CREATE TABLE `+pq.QuoteIdentifier(schema)+`.forbidden_native_control(id integer)`)
		var pgError *pq.Error
		require.ErrorAs(t, err, &pgError)
		require.Equal(t, pq.ErrorCode("42501"), pgError.Code)
	}
}

func TestPostgresNativeControlRejectCollisionRollsBackRealDenial(t *testing.T) {
	f, request, before, _, _ := nativeControlFixture(t)
	changed := request
	changed.Subject = strings.Repeat("f", 64)
	original, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, changed))
	require.NoError(t, err)
	_, err = f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	requireCode(t, err, "control_input_conflict")
	// Bypass only the retained-input precheck to force the public SDK race fence.
	// The normal command row lock serializes Engine callers before this point.
	tx, err := f.engine.beginCommand(t.Context(), request.RolloutID)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	command, err := f.engine.controlScope(t.Context(), tx, request)
	require.NoError(t, err)
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	attempted, err := f.engine.recordControl(t.Context(), tx, command, request, payload)
	require.NoError(t, err)
	require.Equal(t, "applied", receiptResult(t, attempted).Outcome)
	client, cancel := f.engine.transactionClient(t.Context())
	defer cancel()
	_, err = dbos.Enqueue[signedcontrol.Receipt](client, controlReceiptQueue, controlReceiptWorkflow, attempted, dbos.WithEnqueueWorkflowID(f.engine.controlWorkflowID(request.ID)), dbos.WithEnqueueApplicationName(controlApplication), dbos.WithEnqueueWorkflowIDReusePolicy(dbos.WorkflowIDReusePolicyReject), dbos.WithEnqueueTransaction(tx))
	require.ErrorIs(t, err, dbos.ErrWorkflowIDInUse)
	require.NoError(t, tx.Rollback())
	after, err := f.engine.Get(t.Context(), request.RolloutID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	retained, err := f.engine.readControl(t.Context(), request.ID)
	require.NoError(t, err)
	require.Equal(t, original, *retained)
	var gates, decisions int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_environments WHERE rollout=$1`, request.RolloutID).Scan(&gates))
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_decisions WHERE rollout=$1`, request.RolloutID).Scan(&decisions))
	require.Equal(t, 1, gates)
	require.Zero(t, decisions)
}
