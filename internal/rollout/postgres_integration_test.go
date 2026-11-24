//go:build integration

package rollout

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/lib/pq"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stretchr/testify/require"
)

// This suite is an explicit qualification gate, not an optional unit-test skip.
func postgresFixture(t *testing.T, strategy string) (*fixture, string, *sql.DB) {
	t.Helper()
	_, toolErr := exec.LookPath("tofu")
	require.NoError(t, toolErr, "real OpenTofu is required for PostgreSQL qualification")
	f, trace, _ := canonicalFixture(t, strategy, "opentofu")
	return f, trace, openPostgresFixture(t, f)
}

func openPostgresFixture(t *testing.T, f *fixture) *sql.DB {
	t.Helper()
	path := os.Getenv("RUNNER_POSTGRES_TEST_DSN_FILE")
	require.NotEmpty(t, path, "real PostgreSQL DSN secret file is required")
	dsn, err := readStorageSecret(path, 8192)
	require.NoError(t, err)
	admin, err := sql.Open("postgres", string(dsn))
	require.NoError(t, err)
	require.NoError(t, admin.PingContext(t.Context()))
	closeFixture(t, f.engine)
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	key := filepath.Join(t.TempDir(), "shared.agekey")
	require.NoError(t, os.WriteFile(key, []byte(identity.String()), 0400))
	suffix := "runner_test_" + uuid.NewString()[:8]
	f.config.Directory = filepath.Join(t.TempDir(), "worker")
	f.config.RunnerUUID = uuid.NewString()
	f.command.Version, f.command.RunnerUUID, f.command.Request.RunnerUUID = NativePipelineVersion, f.config.RunnerUUID, f.config.RunnerUUID
	f.command.ExecutionLimits = &ExecutionLimits{PlanSeconds: 1800, ApplySeconds: 7200, TaskSeconds: 1800}
	f.config.Postgres = &PostgresConfig{Concurrency: 4, WorkerConcurrency: 2, DSNFile: path, IdentityFile: key, WorkerID: "worker-0", ApplicationSchema: suffix, SystemSchema: suffix + "_dbos"}
	t.Cleanup(func() {
		if f.engine != nil {
			closeFixture(t, f.engine)
		}
		_, err := admin.Exec(`DROP SCHEMA IF EXISTS ` + pq.QuoteIdentifier(suffix) + ` CASCADE; DROP SCHEMA IF EXISTS ` + pq.QuoteIdentifier(suffix+"_dbos") + ` CASCADE`)
		require.NoError(t, err)
		require.NoError(t, admin.Close())
	})
	require.NoError(t, InitializePostgres(t.Context(), f.config))
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	f.command.ToolsDigest = f.engine.ToolsDigest()
	f.command.Request.DeploymentToken, f.command.Request.MetadataKey = "", ""
	// This is a standalone Engine fixture, not a Core-registered local backend.
	f.command.NativeTarget = &NativeExecutionTarget{RunnerID: f.command.RunnerID, RunnerUUID: f.command.RunnerUUID, RuntimeVersion: NativeRuntimeVersion, PipelineVersion: NativePipelineVersion, ToolsDigest: f.command.ToolsDigest, StateBackendType: f.command.Request.Backend, StateConfigurationDigest: strings.Repeat("b", 64)}
	return admin
}

func reopenPostgres(t *testing.T, f *fixture) {
	t.Helper()
	closeFixture(t, f.engine)
	var err error
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
}

func TestPostgresRetainsExactPlanAndEncryptedDelivery(t *testing.T) {
	f, trace, _ := postgresFixture(t, "plan")
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	child := savedCommand(t, f, f.command, "saved-child")
	reopenPostgres(t, f)
	_, err = f.engine.Submit(t.Context(), f.signed(t, child))
	require.NoError(t, err)
	completed := waitPhase(t, f.engine, child.ID, "completed")
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	last := events[len(events)-1]
	require.Equal(t, completed.Revision, last.Progress.Revision)
	frozen, err := f.engine.PrepareDelivery(t.Context(), last.ID)
	require.NoError(t, err)
	require.NotEmpty(t, frozen.Result)
	reopenPostgres(t, f)
	replay, err := f.engine.PrepareDelivery(t.Context(), last.ID)
	require.NoError(t, err)
	require.Equal(t, frozen, replay)
	_, err = f.engine.Submit(t.Context(), f.signed(t, savedCommand(t, f, f.command, "second-child")))
	requireCode(t, err, "saved_plan_already_used")
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	logs, err := f.engine.GetExecutionLogs(t.Context(), child.ID, "")
	require.NoError(t, err)
	require.Contains(t, string(logs.Content), "apply_complete")
	var dump string
	err = f.engine.db.QueryRow(`SELECT row_to_json(c)::text FROM rollout_commands c WHERE id=$1`, f.command.ID).Scan(&dump)
	require.NoError(t, err)
	require.NotContains(t, dump, "PRIVATE_PIPELINE_VALUE")
	var stored []byte
	err = f.engine.db.QueryRow(`SELECT payload FROM rollout_commands WHERE id=$1`, f.command.ID).Scan(&stored)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(stored, []byte("age-encryption.org/v1")))
	var delivery []byte
	require.NoError(t, f.engine.db.QueryRow(`SELECT content FROM rollout_deliveries WHERE event_id=$1`, last.ID).Scan(&delivery))
	require.True(t, bytes.HasPrefix(delivery, []byte("age-encryption.org/v1")))
	require.NotContains(t, string(delivery), "PRIVATE_PIPELINE_VALUE")
	rows, err := f.engine.db.Query(`SELECT plan FROM rollout_artifacts UNION ALL SELECT review FROM rollout_artifacts UNION ALL SELECT outputs FROM rollout_artifacts UNION ALL SELECT body FROM rollout_pipeline_results UNION ALL SELECT outputs FROM rollout_pipeline_results UNION ALL SELECT content FROM rollout_plan_logs UNION ALL SELECT content FROM rollout_execution_logs`)
	require.NoError(t, err)
	for rows.Next() {
		var ciphertext []byte
		require.NoError(t, rows.Scan(&ciphertext))
		if len(ciphertext) > 0 {
			require.True(t, bytes.HasPrefix(ciphertext, []byte("age-encryption.org/v1")))
			require.NotContains(t, string(ciphertext), "PRIVATE_PIPELINE_VALUE")
		}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NoError(t, f.engine.AckEvent(t.Context(), last.ID))
}

func TestPostgresApprovalAndNoEffectAuthority(t *testing.T) {
	f, _, _ := postgresFixture(t, "reviewed")
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	_, err = f.engine.Decide(t.Context(), p.ID, p.Subject, "deny", "reviewer")
	require.NoError(t, err)
	p, err = f.engine.Get(t.Context(), p.ID)
	require.NoError(t, err)
	require.Equal(t, "denied", p.Phase)
	_, body, ready, err := f.engine.Completion(t.Context(), p)
	require.NoError(t, err)
	require.True(t, ready, "native denied command must publish its terminal result")
	var completion api.DeploymentResultsUpdateBody
	require.NoError(t, json.Unmarshal(body, &completion))
	require.NotNil(t, completion.NoEffectReceipt)
	require.Equal(t, api.NotStarted, completion.NoEffectReceipt.Effect)
	tx, err := f.engine.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	receipt, err := f.engine.noEffectReceipt(t.Context(), tx, f.command)
	require.NoError(t, err)
	require.NotNil(t, receipt)
	require.NoError(t, tx.Rollback())
	reopenPostgres(t, f)
	_, err = f.engine.db.Exec(`ALTER TABLE rollout_no_effect_blocks ADD CONSTRAINT refuse_test_claim CHECK(rollout<>'rollout-1')`)
	require.NoError(t, err)
	tx, err = f.engine.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.Error(t, insertEffectClaim(t.Context(), tx, f.command.ID, "rolled-back"))
	require.NoError(t, tx.Rollback())
	var claims int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects`).Scan(&claims))
	require.Zero(t, claims, "failed sticky evidence insert committed the effect claim")
	_, err = f.engine.db.Exec(`ALTER TABLE rollout_no_effect_blocks DROP CONSTRAINT refuse_test_claim`)
	require.NoError(t, err)
	tx, err = f.engine.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, insertEffectClaim(t.Context(), tx, f.command.ID, "test"))
	require.NoError(t, tx.Commit())
	reopenPostgres(t, f)
	tx, err = f.engine.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	receipt, err = f.engine.noEffectReceipt(t.Context(), tx, f.command)
	require.NoError(t, err)
	require.Nil(t, receipt)
	require.NoError(t, tx.Rollback())
}

func TestPostgresReconnectCannotWriteThroughReplacement(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	otherConfig := f.config
	pg := *f.config.Postgres
	otherConfig.Postgres = &pg
	otherConfig.Directory = filepath.Join(t.TempDir(), "replacement")
	executor := *f.config.Executor
	otherConfig.Executor = &executor
	other, err := Open(t.Context(), otherConfig)
	require.Nil(t, other)
	require.Error(t, err)
	var permanent *Error
	require.False(t, errors.As(err, &permanent))
	var pid int
	require.NoError(t, f.engine.db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	var killed bool
	require.NoError(t, admin.QueryRow(`SELECT pg_terminate_backend($1,5000)`, pid).Scan(&killed))
	require.True(t, killed)
	other, err = Open(t.Context(), otherConfig)
	require.NoError(t, err)
	defer closeFixture(t, other)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = f.engine.db.ExecContext(ctx, `INSERT INTO rollout_commands(id,digest,payload,progress,version,environment,strategy) VALUES('stale','x','x','{}','x','x','x')`)
	require.Error(t, err)
	require.False(t, errors.As(err, &permanent))
	require.False(t, f.engine.Ready())
	var count int
	require.NoError(t, other.db.QueryRow(`SELECT count(*) FROM rollout_commands WHERE id='stale'`).Scan(&count))
	require.Zero(t, count)
}

func TestPostgresRefusesWrongAuthorityBeforeRecovery(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	closeFixture(t, f.engine)
	original := *f.config.Postgres
	wrong, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	wrongPath := filepath.Join(t.TempDir(), "wrong-key")
	require.NoError(t, os.WriteFile(wrongPath, []byte(wrong.String()), 0400))
	for _, test := range []struct {
		name, code string
		change     func(*Config)
	}{
		{"missing_worker", "postgres_configuration_refused", func(c *Config) { c.Postgres.WorkerID = "" }},
		{"missing_key", "storage_identity_unavailable", func(c *Config) { c.Postgres.IdentityFile += ".missing" }},
		{"wrong_key", "catalog_refused", func(c *Config) { c.Postgres.IdentityFile = wrongPath }},
		{"wrong_uuid", "site_identity_conflict", func(c *Config) { c.RunnerUUID = uuid.NewString() }},
		{"wrong_site", "site_identity_conflict", func(c *Config) { c.RunnerID = "another-site" }},
		{"wrong_system_schema", "postgres_storage_format_refused", func(c *Config) { c.Postgres.SystemSchema += "_wrong" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := f.config
			pg := original
			cfg.Postgres = &pg
			test.change(&cfg)
			engine, err := Open(t.Context(), cfg)
			require.Nil(t, engine)
			requireCode(t, err, test.code)
		})
	}
	f.config.Executor.Capabilities = []string{"changed-catalogue"}
	engine, err := Open(t.Context(), f.config)
	require.Nil(t, engine)
	requireCode(t, err, "catalog_refused")
	f.config.Executor.Capabilities = nil
	_, err = admin.Exec(`UPDATE ` + pq.QuoteIdentifier(original.ApplicationSchema) + `.rollout_storage SET version=999`)
	require.NoError(t, err)
	engine, err = Open(t.Context(), f.config)
	require.Nil(t, engine)
	requireCode(t, err, "postgres_storage_format_refused")
	var workflows int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(original.SystemSchema)+`.workflow_status`).Scan(&workflows))
	require.Zero(t, workflows)
}

func TestPostgresRuntimeDoesNotInitializeOrRepairSchema(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	closeFixture(t, f.engine)
	name := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	_, err := admin.Exec(`ALTER TABLE ` + name + `.rollout_effects DROP CONSTRAINT rollout_effects_pkey`)
	require.NoError(t, err)
	engine, err := Open(t.Context(), f.config)
	require.Nil(t, engine)
	requireCode(t, err, "postgres_storage_format_refused")
	_, err = admin.Exec(`DROP SCHEMA ` + name + ` CASCADE`)
	require.NoError(t, err)
	engine, err = Open(t.Context(), f.config)
	require.Nil(t, engine)
	requireCode(t, err, "postgres_storage_format_refused")
	var exists bool
	require.NoError(t, admin.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, f.config.Postgres.ApplicationSchema).Scan(&exists))
	require.False(t, exists, "runtime recreated the missing schema")
}

func TestPostgresRejectsSQLiteChangeoverAndSecretDisclosure(t *testing.T) {
	f, _, _ := postgresFixture(t, "plan")
	closeFixture(t, f.engine)
	marker := filepath.Join(f.config.Directory, "rollouts.sqlite-wal")
	require.NoError(t, os.WriteFile(marker, []byte("old-history"), 0600))
	engine, err := Open(t.Context(), f.config)
	require.Nil(t, engine)
	requireCode(t, err, "postgres_requires_fresh_directory")
	stored, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "old-history", string(stored))
	require.NoError(t, os.Remove(marker))
	bad := filepath.Join(t.TempDir(), "invalid-dsn")
	require.NoError(t, os.WriteFile(bad, []byte("postgres://PRIVATE_DSN_CANARY:%ZZ@localhost/db"), 0600))
	f.config.Postgres.DSNFile = bad
	engine, err = Open(t.Context(), f.config)
	require.Nil(t, engine)
	requireCode(t, err, "postgres_configuration_refused")
	require.NotContains(t, err.Error(), "PRIVATE_DSN_CANARY")
}

func TestPostgresSavedPlanHasOneConcurrentConsumer(t *testing.T) {
	f, trace, _ := postgresFixture(t, "plan")
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	first := savedCommand(t, f, f.command, "first-consumer")
	second := savedCommand(t, f, f.command, "second-consumer")
	results := make(chan error, 2)
	for _, command := range []Command{first, second} {
		signed := f.signed(t, command)
		go func() { _, err := f.engine.importCommand(t.Context(), command, signed.Payload); results <- err }()
	}
	a, b := <-results, <-results
	if a != nil {
		a, b = b, a
	}
	require.NoError(t, a)
	requireCode(t, b, "saved_plan_already_used")
	var uses int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_plan_uses`).Scan(&uses))
	require.Equal(t, 1, uses)
	var winner string
	require.NoError(t, f.engine.db.QueryRow(`SELECT rollout FROM rollout_plan_uses`).Scan(&winner))
	waitPhase(t, f.engine, winner, "completed")
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

func TestPostgresReviewedApprovalResumesExactApply(t *testing.T) {
	f, trace, _ := postgresFixture(t, "reviewed")
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	reopenPostgres(t, f)
	_, err = f.engine.Decide(t.Context(), p.ID, p.Subject, "approve", "reviewer")
	require.NoError(t, err)
	p = waitPhase(t, f.engine, p.ID, "completed")
	require.True(t, p.InfrastructureApplied)
	require.Equal(t, "reviewer", p.ApprovalDecision.ActorID)
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

func TestPostgresInitializationMustFinishBeforeRecovery(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	closeFixture(t, f.engine)
	app := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	_, err := admin.Exec(`UPDATE ` + app + `.rollout_storage SET workflow_ready=0`)
	require.NoError(t, err)
	engine, err := Open(t.Context(), f.config)
	require.Nil(t, engine)
	requireCode(t, err, "postgres_initialization_incomplete")
	require.NoError(t, InitializePostgres(t.Context(), f.config))
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	closeFixture(t, f.engine)
	_, err = admin.Exec(`DROP SCHEMA ` + app + ` CASCADE`)
	require.NoError(t, err)
	err = InitializePostgres(t.Context(), f.config)
	requireCode(t, err, "postgres_existing_workflow_storage_refused")
	var exists bool
	require.NoError(t, admin.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, f.config.Postgres.ApplicationSchema).Scan(&exists))
	require.False(t, exists, "initialization attached unknown DBOS history")
}

func TestPostgresRuntimeRoleNeedsNoDDL(t *testing.T) {
	f, _, admin := postgresFixture(t, "reviewed")
	closeFixture(t, f.engine)
	role := "runner_role_" + uuid.NewString()[:8]
	password := uuid.NewString()
	_, err := admin.Exec(`CREATE ROLE ` + pq.QuoteIdentifier(role) + ` LOGIN PASSWORD ` + pq.QuoteLiteral(password) + ` NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if f.engine != nil {
			closeFixture(t, f.engine)
		}
		_, err := admin.Exec(`DROP OWNED BY ` + pq.QuoteIdentifier(role) + `; DROP ROLE ` + pq.QuoteIdentifier(role))
		require.NoError(t, err)
	})
	quotedRole := pq.QuoteIdentifier(role)
	for _, schema := range []string{f.config.Postgres.ApplicationSchema, f.config.Postgres.SystemSchema} {
		name := pq.QuoteIdentifier(schema)
		_, err = admin.Exec(`GRANT USAGE ON SCHEMA ` + name + ` TO ` + quotedRole + `;
 GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA ` + name + ` TO ` + quotedRole + `;
 GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA ` + name + ` TO ` + quotedRole + `;
 GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA ` + name + ` TO ` + quotedRole)
		require.NoError(t, err)
	}
	rawDSN, err := readStorageSecret(f.config.Postgres.DSNFile, 8192)
	require.NoError(t, err)
	dsn := string(rawDSN)
	path := filepath.Join(t.TempDir(), "runtime-dsn")
	require.NoError(t, os.WriteFile(path, []byte(dsn+" user="+role+" password="+password), 0600))
	f.config.Postgres.DSNFile = path
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	limited, err := sql.Open("postgres", dsn+" user="+role+" password="+password)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, limited.Close()) })
	var currentUser string
	var superuser, createDB, createRole, inherit bool
	require.NoError(t, limited.QueryRow(`SELECT current_user,rolsuper,rolcreatedb,rolcreaterole,rolinherit FROM pg_roles WHERE rolname=current_user`).Scan(&currentUser, &superuser, &createDB, &createRole, &inherit))
	require.Equal(t, role, currentUser)
	require.False(t, superuser)
	require.False(t, createDB)
	require.False(t, createRole)
	require.False(t, inherit)
	require.NoError(t, f.engine.db.QueryRowContext(t.Context(), `SELECT current_user`).Scan(&currentUser))
	require.Equal(t, role, currentUser, "the worker must use the restricted role too")
	for _, target := range []struct{ schema, table string }{
		{f.config.Postgres.ApplicationSchema, "rollout_site"},
		{f.config.Postgres.SystemSchema, "workflow_status"},
	} {
		table := pq.QuoteIdentifier(target.schema) + "." + pq.QuoteIdentifier(target.table)
		var usage, createSchema, truncate, trigger, references bool
		require.NoError(t, limited.QueryRow(`SELECT has_schema_privilege(current_user,$1,'USAGE'),has_schema_privilege(current_user,$1,'CREATE'),
 has_table_privilege(current_user,$2,'TRUNCATE'),has_table_privilege(current_user,$2,'TRIGGER'),has_table_privilege(current_user,$2,'REFERENCES')`, target.schema, table).Scan(&usage, &createSchema, &truncate, &trigger, &references))
		require.True(t, usage)
		require.False(t, createSchema)
		require.False(t, truncate)
		require.False(t, trigger)
		require.False(t, references)
		for _, statement := range []string{
			`CREATE TABLE ` + pq.QuoteIdentifier(target.schema) + `.runtime_must_not_create(id INTEGER)`,
			`ALTER TABLE ` + table + ` ADD COLUMN runtime_must_not_alter INTEGER`,
			`DROP TABLE ` + table,
		} {
			_, err = limited.Exec(statement)
			var denied *pq.Error
			require.ErrorAs(t, err, &denied)
			require.Equal(t, "42501", denied.SQLState(), "DDL must fail because of privileges")
		}
	}
	_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	_, err = f.engine.Decide(t.Context(), p.ID, p.Subject, "approve", "reviewer")
	require.NoError(t, err)
	waitPhase(t, f.engine, p.ID, "completed")
}
