//go:build integration

package rollout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/golib/hmessaging"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func TestPostgresNativeUUIDSeparatesSameSlugPools(t *testing.T) {
	f, trace, admin := postgresFixture(t, "plan")
	for _, test := range []struct {
		name   string
		change func(*Command)
	}{
		{"ancient_v1", func(c *Command) { c.Version = Version }}, {"pipeline_v1", func(c *Command) { c.Version = PipelineVersion }},
		{"outer_missing", func(c *Command) { c.RunnerUUID = "" }}, {"inner_missing", func(c *Command) { c.Request.RunnerUUID = "" }},
		{"outer_foreign", func(c *Command) { c.RunnerUUID = uuid.NewString() }}, {"inner_foreign", func(c *Command) { c.Request.RunnerUUID = uuid.NewString() }},
		{"both_foreign", func(c *Command) { c.RunnerUUID = uuid.NewString(); c.Request.RunnerUUID = c.RunnerUUID }},
		{"uppercase", func(c *Command) { c.RunnerUUID = strings.ToUpper(c.RunnerUUID); c.Request.RunnerUUID = c.RunnerUUID }},
		{"nil", func(c *Command) { c.RunnerUUID = uuid.Nil.String(); c.Request.RunnerUUID = c.RunnerUUID }},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := f.command
			test.change(&command)
			_, err := f.engine.Submit(t.Context(), f.signed(t, command))
			code := "command_scope_refused"
			if strings.HasPrefix(test.name, "inner_") {
				code = "request_scope_refused"
			}
			requireCode(t, err, code)
		})
	}
	var commands, effects int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_commands`).Scan(&commands))
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects`).Scan(&effects))
	require.Zero(t, commands)
	require.Zero(t, effects)
	// Deliberately share slug, verification key, encryption key and tools. UUID is the distinguishing authority.
	cfg := f.config
	pg := *cfg.Postgres
	executor := *cfg.Executor
	cfg.Postgres, cfg.Executor = &pg, &executor
	cfg.RunnerUUID = uuid.NewString()
	cfg.Directory = filepath.Join(t.TempDir(), "other-pool")
	pg.ApplicationSchema = "binding_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	pg.SystemSchema = pg.ApplicationSchema + "_dbos"
	require.NoError(t, InitializePostgres(t.Context(), cfg))
	other, err := Open(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeFixture(t, other)
		_, dropErr := admin.Exec(`DROP SCHEMA ` + pq.QuoteIdentifier(pg.ApplicationSchema) + ` CASCADE; DROP SCHEMA ` + pq.QuoteIdentifier(pg.SystemSchema) + ` CASCADE`)
		require.NoError(t, dropErr)
	})
	_, err = other.Submit(t.Context(), f.signed(t, f.command))
	requireCode(t, err, "command_scope_refused")
	otherCommand := f.command
	otherCommand.RunnerUUID, otherCommand.Request.RunnerUUID = cfg.RunnerUUID, cfg.RunnerUUID
	target := *otherCommand.NativeTarget
	target.RunnerUUID = cfg.RunnerUUID
	otherCommand.NativeTarget = &target
	_, err = f.engine.Submit(t.Context(), f.signed(t, otherCommand))
	requireCode(t, err, "command_scope_refused")
	// Same inner command ID can be admitted independently only with each pool's own signed UUID.
	f.command.ExpiresAt = time.Now().Add(2 * time.Second)
	signed := f.signed(t, f.command)
	_, err = f.engine.Submit(t.Context(), signed)
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	_, err = other.Submit(t.Context(), f.signed(t, otherCommand))
	require.NoError(t, err)
	waitPhase(t, other, otherCommand.ID, "completed")
	if delay := time.Until(f.command.ExpiresAt); delay > 0 {
		time.Sleep(delay + time.Millisecond)
	}
	f.engine.executor.Capabilities = append(f.engine.executor.Capabilities, "changed-after-acceptance")
	_, err = f.engine.Submit(t.Context(), signed)
	require.NoError(t, err, "exact replay precedes expiry/current inventory checks")
	require.Zero(t, operationCount(t, trace, "apply"))
	var runtime, name string
	require.NoError(t, admin.QueryRow(`SELECT application_version,name FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.workflow_status WHERE workflow_uuid=$1`, f.command.ID).Scan(&runtime, &name))
	require.Equal(t, NativeRuntimeVersion, runtime)
	require.Equal(t, NativeRuntimeVersion, name)
}

func TestPostgresNativeRefusesOldFormatWithoutRepair(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	closeFixture(t, f.engine)
	// Reconstruct the exact unbound format2 schema/metadata, retaining its owned key and DBOS journal.
	oldSchema := strings.Replace(postgresSchema, ", runner_uuid UUID NOT NULL", "", 1)
	_, err := f.engine.db.Exec(`SELECT 1`)
	require.Error(t, err, "fixture Engine must be closed before old-format reconstruction")
	table := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	tx, err := admin.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`SET LOCAL search_path=` + table)
	require.NoError(t, err)
	_, err = tx.Exec(`ALTER TABLE rollout_site DROP COLUMN runner_uuid`)
	require.NoError(t, err)
	structure, err := postgresStructure(t.Context(), tx)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE rollout_storage SET version=2,application=$1,schema_digest=$2,structure_digest=$3`, RuntimeVersion, digest([]byte(oldSchema)), structure)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var before string
	require.NoError(t, admin.QueryRow(`SELECT row_to_json(s)::text FROM `+table+`.rollout_storage s`).Scan(&before))
	for _, initialize := range []bool{false, true} {
		if initialize {
			err = InitializePostgres(t.Context(), f.config)
		} else {
			var engine *Engine
			engine, err = Open(t.Context(), f.config)
			require.Nil(t, engine)
		}
		requireCode(t, err, "postgres_storage_format_refused")
	}
	var after string
	require.NoError(t, admin.QueryRow(`SELECT row_to_json(s)::text FROM `+table+`.rollout_storage s`).Scan(&after))
	require.Equal(t, before, after)
	var uuidColumn bool
	require.NoError(t, admin.QueryRow(`SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='rollout_site' AND column_name='runner_uuid')`, f.config.Postgres.ApplicationSchema).Scan(&uuidColumn))
	require.False(t, uuidColumn, "refused initialization repaired old authority")
}

func TestPostgresStoredForeignUUIDRefusesBeforeRecovery(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	command := f.command
	command.RunnerUUID = uuid.NewString()
	command.Request.RunnerUUID = command.RunnerUUID
	plain, err := json.Marshal(command)
	require.NoError(t, err)
	encrypted, err := f.engine.encrypt(plain)
	require.NoError(t, err)
	_, err = f.engine.db.Exec(`INSERT INTO rollout_commands(id,digest,payload,progress,version,environment,strategy) VALUES($1,$2,$3,$4,$5,$6,$7)`, command.ID, digest(plain), encrypted, []byte(`{}`), command.Version, command.EnvironmentID, command.Strategy)
	require.NoError(t, err)
	closeFixture(t, f.engine)
	engine, err := Open(t.Context(), f.config)
	require.Nil(t, engine)
	requireCode(t, err, "stored_command_unavailable")
	var workflows int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.workflow_status`).Scan(&workflows))
	require.Zero(t, workflows, "invalid stored scope reached DBOS Launch/recovery")
}

func TestPostgresNativeStrategiesProduceResultsAndLogs(t *testing.T) {
	for _, strategy := range []string{"apply", "destroy", "observation"} {
		t.Run(strategy, func(t *testing.T) {
			f, trace, _ := canonicalFixture(t, strategy, "opentofu")
			if strategy == "observation" {
				f.command.Request.Mode = "observation"
				f.command.Request.EncryptedOutputsRecipient = ""
				f.config.Executor.Capabilities = []string{rolloutexec.ObservationCapability}
				f.command.RequiredCapabilities = f.config.Executor.Capabilities
			}
			f.command.ToolsDigest = f.config.Executor.CatalogDigest()
			openPostgresFixture(t, f)
			_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
			require.NoError(t, err)
			completed := waitPhase(t, f.engine, f.command.ID, "completed")
			_, body, ready, err := f.engine.Completion(t.Context(), completed)
			require.NoError(t, err)
			require.True(t, ready)
			var result api.DeploymentResultsUpdateBody
			require.NoError(t, json.Unmarshal(body, &result))
			require.Equal(t, api.Success, result.Status)
			if strategy == "observation" {
				require.NotNil(t, result.ObservationResult)
				require.Zero(t, operationCount(t, trace, "apply"))
			} else {
				require.Equal(t, 1, operationCount(t, trace, strategy))
				logs, logErr := f.engine.GetExecutionLogs(t.Context(), f.command.ID, "")
				require.NoError(t, logErr)
				require.NotEmpty(t, logs.Content)
			}
		})
	}
}

func TestPostgresNativePublishedSavedPlanKeepsApprovalAndLogs(t *testing.T) {
	f, trace := publishedFixture(t, "plan", "opentofu")
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	f.command.Approval = Approval{Mode: "local", PolicyID: f.command.Recipe.ID, PolicyVersion: "1"}
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
	openPostgresFixture(t, f)
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	source := waitPhase(t, f.engine, f.command.ID, "completed")
	logs, err := f.engine.GetExecutionLogs(t.Context(), source.ID, "")
	require.NoError(t, err)
	require.NotEmpty(t, logs.Content)
	child := savedCommand(t, f, f.command, "native-child")
	_, err = f.engine.Submit(t.Context(), f.signed(t, child))
	require.NoError(t, err)
	pending := waitPhase(t, f.engine, child.ID, "awaiting_approval")
	_, err = f.engine.Decide(t.Context(), child.ID, pending.Subject, "approve", "native-reviewer")
	require.NoError(t, err)
	completed := waitPhase(t, f.engine, child.ID, "completed")
	require.Equal(t, "native-reviewer", completed.ApprovalDecision.ActorID)
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	_, err = f.engine.GetExecutionLogs(t.Context(), child.ID, "")
	require.NoError(t, err)
}

func TestPostgresNativeSupersededPlanCannotApply(t *testing.T) {
	f, trace, _ := postgresFixture(t, "plan")
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	newer := f.command
	newer.ID, newer.Request.ID = "native-newer", "native-newer"
	newer.Strategy, newer.Request.Mode, newer.PlanBase = "apply", "deploy", nil
	_, err = f.engine.Submit(t.Context(), f.signed(t, newer))
	require.NoError(t, err)
	waitPhase(t, f.engine, newer.ID, "completed")
	_, err = f.engine.Submit(t.Context(), f.signed(t, savedCommand(t, f, f.command, "stale-child")))
	requireCode(t, err, "saved_plan_superseded")
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	_, statErr := os.Stat(f.state)
	require.NoError(t, statErr)
}

func TestPostgresNativeRefusesSlugDeliveryJournalWithoutMutation(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	frozen, err := f.engine.PrepareDelivery(t.Context(), events[0].ID)
	require.NoError(t, err)
	var envelope hmessaging.EventEnvelope
	require.NoError(t, json.Unmarshal(frozen.Progress, &envelope))
	token, err := hmessaging.NativeRunnerToken(f.config.RunnerUUID)
	require.NoError(t, err)
	require.Equal(t, token, envelope.RunnerID)
	// Format3 retained the human slug in an already frozen encrypted delivery.
	envelope.RunnerID = f.config.RunnerID
	frozen.Progress, err = json.Marshal(envelope)
	require.NoError(t, err)
	plain, err := json.Marshal(frozen)
	require.NoError(t, err)
	old, err := f.engine.encrypt(plain)
	require.NoError(t, err)
	_, err = f.engine.db.Exec(`UPDATE rollout_deliveries SET content=$1 WHERE event_id=$2`, old, events[0].ID)
	require.NoError(t, err)
	closeFixture(t, f.engine)
	table := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	_, err = admin.Exec(`UPDATE ` + table + `.rollout_storage SET version=3`)
	require.NoError(t, err)
	var before string
	require.NoError(t, admin.QueryRow(`SELECT row_to_json(s)::text FROM `+table+`.rollout_storage s`).Scan(&before))
	for _, initialize := range []bool{false, true} {
		if initialize {
			err = InitializePostgres(t.Context(), f.config)
		} else {
			var engine *Engine
			engine, err = Open(t.Context(), f.config)
			require.Nil(t, engine)
		}
		requireCode(t, err, "postgres_storage_format_refused")
	}
	var after string
	var retained []byte
	require.NoError(t, admin.QueryRow(`SELECT row_to_json(s)::text FROM `+table+`.rollout_storage s`).Scan(&after))
	require.Equal(t, before, after)
	require.NoError(t, admin.QueryRow(`SELECT content FROM `+table+`.rollout_deliveries WHERE event_id=$1`, events[0].ID).Scan(&retained))
	require.Equal(t, old, retained, "refusal must not rewrite a frozen slug delivery")
}
