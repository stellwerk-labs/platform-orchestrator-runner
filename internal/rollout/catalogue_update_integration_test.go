//go:build integration

package rollout

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func catalogueUpdateConfig(t *testing.T, cfg Config) Config {
	t.Helper()
	pg := *cfg.Postgres
	execution := *cfg.Executor
	execution.Capabilities = append(append([]string{}, execution.Capabilities...), "catalogue-update-trial")
	cfg.Postgres, cfg.Executor = &pg, &execution
	return cfg
}

// Compare every retained table in both schemas. Only the two maintenance-owned
// fields are projected out; ciphertext, result delivery and DBOS input remain exact.
func catalogueHistory(t *testing.T, db *sql.DB, cfg Config) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT n.nspname,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ($1,$2) AND c.relkind='r' ORDER BY n.nspname,c.relname`, cfg.Postgres.ApplicationSchema, cfg.Postgres.SystemSchema)
	require.NoError(t, err)
	var names [][2]string
	for rows.Next() {
		var name [2]string
		require.NoError(t, rows.Scan(&name[0], &name[1]))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	result := map[string]string{}
	for _, name := range names {
		projection := "to_jsonb(t)"
		if name[0] == cfg.Postgres.ApplicationSchema {
			switch name[1] {
			case "rollout_storage":
				projection += "-'catalogue'"
			case "rollout_workers":
				projection += "-'generation'"
			}
		}
		var value string
		query := `SELECT coalesce(jsonb_agg(v ORDER BY v::text)::text,'[]') FROM (SELECT ` + projection + ` AS v FROM ` + pq.QuoteIdentifier(name[0]) + `.` + pq.QuoteIdentifier(name[1]) + ` t) s`
		require.NoError(t, db.QueryRow(query).Scan(&value))
		result[name[0]+"."+name[1]] = value
	}
	return result
}

func catalogueGenerations(t *testing.T, db *sql.DB, cfg Config) string {
	t.Helper()
	var result string
	require.NoError(t, db.QueryRow(`SELECT coalesce(jsonb_agg(to_jsonb(w) ORDER BY worker)::text,'[]') FROM `+pq.QuoteIdentifier(cfg.Postgres.ApplicationSchema)+`.rollout_workers w`).Scan(&result))
	return result
}

func TestPostgresCatalogueUpdatePreservesExecutionAndSavedPlan(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "plan", "opentofu")
	addTask(t, f, "plan_check", "read_only", traceTask(filepath.Join(t.TempDir(), "check-trace")))
	f.config.Executor.Tools[0].Environment = map[string]string{"PRIVATE_CATALOGUE_VALUE": "old-private-value"}
	f.command.Steps = nil
	admin := openPostgresFixture(t, f)
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	signed := f.signed(t, f.command)
	_, err := f.engine.Submit(t.Context(), signed)
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	applied := savedCommand(t, f, f.command, "saved-apply")
	_, err = f.engine.Submit(t.Context(), f.signed(t, applied))
	require.NoError(t, err)
	waitPhase(t, f.engine, applied.ID, "completed")
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	delivery, err := f.engine.PrepareDelivery(t.Context(), events[len(events)-1].ID)
	require.NoError(t, err)
	oldDigest := f.engine.ToolsDigest()
	desired := catalogueUpdateConfig(t, f.config)
	desired.Executor.Tools = append([]rolloutexec.Tool{}, f.config.Executor.Tools...)
	desired.Executor.Tools[0].Environment = map[string]string{"PRIVATE_CATALOGUE_VALUE": "new-private-value"}
	closeFixture(t, f.engine)
	before := catalogueHistory(t, admin, f.config)
	generations := catalogueGenerations(t, admin, f.config)
	require.NoError(t, UpdatePostgresCatalogue(t.Context(), desired, oldDigest, f.config.Postgres.DSNFile))
	require.Equal(t, before, catalogueHistory(t, admin, f.config))
	require.NotEqual(t, generations, catalogueGenerations(t, admin, f.config))
	updated := catalogueGenerations(t, admin, f.config)
	require.NoError(t, UpdatePostgresCatalogue(t.Context(), desired, oldDigest, f.config.Postgres.DSNFile))
	require.Equal(t, updated, catalogueGenerations(t, admin, f.config), "committed retry must not supersede again")
	old, err := Open(t.Context(), f.config)
	require.Nil(t, old)
	requireCode(t, err, "catalog_refused")
	f.config = desired
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	_, err = f.engine.Submit(t.Context(), signed)
	require.NoError(t, err, "known exact replay must survive catalogue replacement")
	replay, err := f.engine.PrepareDelivery(t.Context(), events[len(events)-1].ID)
	require.NoError(t, err)
	require.Equal(t, delivery, replay)
	unseen := f.command
	unseen.ID, unseen.Request.ID = "unseen-old-profile", "unseen-old-profile"
	_, err = f.engine.Submit(t.Context(), f.signed(t, unseen))
	requireCode(t, err, "catalog_refused")
	oldApply := savedCommand(t, f, f.command, "old-plan-new-profile")
	oldApply.ToolsDigest = f.engine.ToolsDigest()
	target := *oldApply.NativeTarget
	target.ToolsDigest = oldApply.ToolsDigest
	oldApply.NativeTarget = &target
	_, err = f.engine.Submit(t.Context(), f.signed(t, oldApply))
	require.Error(t, err, "new profile cannot apply an old saved plan")
	next := unseen
	next.ID, next.Request.ID, next.ToolsDigest = "new-profile-plan", "new-profile-plan", f.engine.ToolsDigest()
	target = *next.NativeTarget
	target.ToolsDigest = next.ToolsDigest
	next.NativeTarget = &target
	_, err = f.engine.Submit(t.Context(), f.signed(t, next))
	require.NoError(t, err)
	waitPhase(t, f.engine, next.ID, "completed")
	require.Equal(t, 2, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

func TestPostgresCatalogueUpdateRetainsPendingControlReceipt(t *testing.T) {
	f, request, _, trace, admin := nativeControlFixture(t)
	signed := signNativeControl(t, f, request)
	receipt, err := f.engine.AcceptControl(t.Context(), signed)
	require.NoError(t, err)
	require.Equal(t, "denied", receiptResult(t, receipt).Phase)
	desired := catalogueUpdateConfig(t, f.config)
	oldDigest := f.engine.ToolsDigest()
	closeFixture(t, f.engine)
	before := catalogueHistory(t, admin, f.config)
	// The runtime is deliberately DML-only. Its separate migration-owner file is
	// required even though its ordinary grants include UPDATE on rollout_storage.
	adminDSN := os.Getenv("RUNNER_POSTGRES_TEST_DSN_FILE")
	requireCode(t, UpdatePostgresCatalogue(t.Context(), desired, oldDigest, f.config.Postgres.DSNFile), "catalogue_update_admin_required")
	require.NoError(t, UpdatePostgresCatalogue(t.Context(), desired, oldDigest, adminDSN))
	require.Equal(t, before, catalogueHistory(t, admin, f.config))
	f.config = desired
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	replay, err := f.engine.AcceptControl(t.Context(), signed)
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
	deliveries, err := f.engine.ControlDeliveries(t.Context(), 0, false)
	require.NoError(t, err)
	require.Len(t, deliveries, 1)
	require.Equal(t, receipt, deliveries[0].Receipt)
	require.Zero(t, operationCount(t, trace, "apply"))
}

func TestPostgresCatalogueUpdateRefusesUnsettledProgressWithoutOwner(t *testing.T) {
	for _, phase := range []string{"accepted", "planning", "awaiting_approval", "post_failed", "post_retrying", "outcome_unknown", "malformed", "inconsistent"} {
		t.Run(phase, func(t *testing.T) {
			f, _, admin := postgresFixture(t, "plan")
			_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
			require.NoError(t, err)
			p := waitPhase(t, f.engine, f.command.ID, "completed")
			desired, oldDigest := catalogueUpdateConfig(t, f.config), f.engine.ToolsDigest()
			closeFixture(t, f.engine)
			p.Phase = phase
			data, err := json.Marshal(p)
			require.NoError(t, err)
			if phase == "malformed" {
				data = []byte("{")
			}
			if phase == "inconsistent" {
				p.ID = "another"
				p.Phase = "completed"
				data, err = json.Marshal(p)
				require.NoError(t, err)
			}
			_, err = admin.Exec(`UPDATE `+pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)+`.rollout_commands SET progress=$1 WHERE id=$2`, data, p.ID)
			if phase == "inconsistent" {
				_, err = admin.Exec(`UPDATE `+pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)+`.rollout_commands SET progress=$1 WHERE id=$2`, data, f.command.ID)
			}
			require.NoError(t, err)
			before := catalogueHistory(t, admin, f.config)
			generations := catalogueGenerations(t, admin, f.config)
			err = UpdatePostgresCatalogue(t.Context(), desired, oldDigest, f.config.Postgres.DSNFile)
			requireCode(t, err, "catalogue_update_not_quiescent")
			require.Equal(t, before, catalogueHistory(t, admin, f.config))
			require.Equal(t, generations, catalogueGenerations(t, admin, f.config))
		})
	}
}

func TestPostgresCatalogueUpdateBusyWorkerDoesNotWaitForAdoption(t *testing.T) {
	f, _, _ := postgresFixture(t, "plan")
	tx, err := f.engine.beginCommand(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	start := time.Now()
	err = UpdatePostgresCatalogue(t.Context(), catalogueUpdateConfig(t, f.config), f.engine.ToolsDigest(), f.config.Postgres.DSNFile)
	requireCode(t, err, "catalogue_update_busy")
	require.Less(t, time.Since(start), 3*time.Second)
	require.NoError(t, tx.Commit())
}

func TestPostgresCatalogueUpdateWholeDeadlineIncludesStorageLock(t *testing.T) {
	f, _, _ := postgresFixture(t, "plan")
	tx, err := f.engine.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var catalogue string
	require.NoError(t, tx.QueryRow(`SELECT catalogue FROM rollout_storage WHERE id=1 FOR SHARE`).Scan(&catalogue))
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = UpdatePostgresCatalogue(ctx, catalogueUpdateConfig(t, f.config), catalogue, f.config.Postgres.DSNFile)
	requireCode(t, err, "catalogue_update_unavailable")
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestPostgresCatalogueUpdateRefusalsPreserveState(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	oldDigest := f.engine.ToolsDigest()
	closeFixture(t, f.engine)
	desired := catalogueUpdateConfig(t, f.config)
	before := catalogueHistory(t, admin, f.config)
	generations := catalogueGenerations(t, admin, f.config)
	for _, test := range []struct {
		name, code string
		mutate     func(*Config, *string)
	}{
		{"bad_cas", "catalogue_update_conflict", func(_ *Config, expected *string) { *expected = strings.Repeat("f", 64) }},
		{"bad_scope", "site_identity_conflict", func(cfg *Config, _ *string) { cfg.OrgID = "other-org" }},
		{"bad_registration", "site_identity_conflict", func(cfg *Config, _ *string) { cfg.RunnerUUID = uuid.NewString() }},
		{"bad_trust", "site_identity_conflict", func(cfg *Config, _ *string) {
			cfg.PublicKey = append([]byte{}, cfg.PublicKey...)
			cfg.PublicKey[0] ^= 1
		}},
		{"bad_key", "storage_identity_unavailable", func(cfg *Config, _ *string) {
			key, err := age.GenerateX25519Identity()
			require.NoError(t, err)
			cfg.Postgres.IdentityFile = filepath.Join(t.TempDir(), "other.agekey")
			require.NoError(t, os.WriteFile(cfg.Postgres.IdentityFile, []byte(key.String()), 0400))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, expected := catalogueUpdateConfig(t, desired), oldDigest
			test.mutate(&cfg, &expected)
			requireCode(t, UpdatePostgresCatalogue(t.Context(), cfg, expected, f.config.Postgres.DSNFile), test.code)
			require.Equal(t, before, catalogueHistory(t, admin, f.config))
			require.Equal(t, generations, catalogueGenerations(t, admin, f.config))
		})
	}
}

// Real subprocesses pause after full binding verification. A generation-zero
// reconnect and a new WorkerID must both recheck storage at atomic activation.
func TestPostgresCatalogueUpdateFencesPausedStartupAndOldProcess(t *testing.T) {
	for _, mode := range []string{"new-slot", "reconnect", "old-process"} {
		t.Run(mode, func(t *testing.T) {
			f, _, admin := postgresFixture(t, "plan")
			oldDigest := f.engine.ToolsDigest()
			cfg := f.config
			pg, execution := *cfg.Postgres, *cfg.Executor
			cfg.Postgres, cfg.Executor = &pg, &execution
			cfg.Executor.Identity = nil
			cfg.Postgres.WorkerID = "paused-" + mode
			folder := t.TempDir()
			encoded, err := json.Marshal(cfg)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(folder, "config.json"), encoded, 0600))
			binary, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, binary, "-test.run=^TestPostgresCatalogueUpdateProcessHelper$")
			child.Env = append(os.Environ(), "RUNNER_CATALOGUE_PROCESS="+folder, "RUNNER_CATALOGUE_PROCESS_MODE="+mode)
			require.NoError(t, child.Start())
			t.Cleanup(func() {
				if child.ProcessState == nil {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			})
			require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(folder, "ready")); return err == nil }, 10*time.Second, 10*time.Millisecond)
			closeFixture(t, f.engine)
			desired := catalogueUpdateConfig(t, f.config)
			require.NoError(t, UpdatePostgresCatalogue(t.Context(), desired, oldDigest, f.config.Postgres.DSNFile))
			require.NoError(t, os.WriteFile(filepath.Join(folder, "release"), []byte("continue"), 0600))
			require.NoError(t, child.Wait())
			var count int
			require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(cfg.Postgres.ApplicationSchema)+`.rollout_effects`).Scan(&count))
			require.Zero(t, count)
			if mode != "old-process" {
				require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(cfg.Postgres.ApplicationSchema)+`.rollout_workers WHERE worker=$1`, cfg.Postgres.WorkerID).Scan(&count))
				require.Zero(t, count, "stale startup created a worker row")
			}
		})
	}
}

func TestPostgresCatalogueUpdateProcessHelper(t *testing.T) {
	folder := os.Getenv("RUNNER_CATALOGUE_PROCESS")
	if folder == "" {
		t.Skip("subprocess helper")
	}
	data, err := os.ReadFile(filepath.Join(folder, "config.json"))
	require.NoError(t, err)
	var cfg Config
	require.NoError(t, json.Unmarshal(data, &cfg))
	e := &Engine{config: cfg, executor: cfg.Executor, failure: make(chan error, 1)}
	require.NoError(t, e.executor.PrepareNativeBackend())
	identity, err := LoadStorageIdentity(cfg.Postgres.IdentityFile)
	require.NoError(t, err)
	e.executor.Identity = identity
	require.NoError(t, e.executor.PrepareNativeCatalogue(identity))
	dsn, err := readPostgresDSN(cfg.Postgres)
	require.NoError(t, err)
	e.db, err = e.openWorkerDatabase(dsn)
	require.NoError(t, err)
	defer func() { _ = e.db.Close() }()
	require.NoError(t, e.verifyPostgresStorage(t.Context(), false))
	mode := os.Getenv("RUNNER_CATALOGUE_PROCESS_MODE")
	if mode == "old-process" {
		require.NoError(t, e.activateSlot(t.Context()))
	}
	if mode == "reconnect" {
		e.db.SetConnMaxLifetime(time.Nanosecond)
	}
	require.NoError(t, os.WriteFile(filepath.Join(folder, "ready"), []byte("verified"), 0600))
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(folder, "release")); return err == nil }, 10*time.Second, 10*time.Millisecond)
	if mode == "old-process" {
		tx, err := e.beginCommand(t.Context())
		require.Nil(t, tx)
		require.ErrorIs(t, err, errGenerationSuperseded)
	} else {
		requireCode(t, e.activateSlot(t.Context()), "catalog_refused")
		require.Zero(t, e.generation.Load())
	}
}

func TestPostgresCatalogueUpdateRollsBackCatalogueWhenGenerationWriteFails(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	oldDigest := f.engine.ToolsDigest()
	closeFixture(t, f.engine)
	app := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	_, err := admin.Exec(`CREATE FUNCTION ` + app + `.refuse_generation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'PRIVATE_SQL_ERROR_CANARY'; END $$;
 CREATE TRIGGER refuse_generation BEFORE UPDATE ON ` + app + `.rollout_workers FOR EACH ROW EXECUTE FUNCTION ` + app + `.refuse_generation()`)
	require.NoError(t, err)
	before := catalogueHistory(t, admin, f.config)
	generations := catalogueGenerations(t, admin, f.config)
	err = UpdatePostgresCatalogue(t.Context(), catalogueUpdateConfig(t, f.config), oldDigest, f.config.Postgres.DSNFile)
	requireCode(t, err, "catalogue_update_unavailable")
	require.NotContains(t, err.Error(), "PRIVATE_SQL_ERROR_CANARY")
	require.Equal(t, before, catalogueHistory(t, admin, f.config))
	require.Equal(t, generations, catalogueGenerations(t, admin, f.config))
	var current string
	require.NoError(t, admin.QueryRow(`SELECT catalogue FROM `+app+`.rollout_storage WHERE id=1`).Scan(&current))
	require.Equal(t, oldDigest, current, "first update must roll back with second update")
}

func TestPostgresCatalogueUpdateRefusesUnknownEffectsAndChangedStructure(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	oldDigest := f.engine.ToolsDigest()
	desired := catalogueUpdateConfig(t, f.config)
	closeFixture(t, f.engine)
	app := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	for _, status := range []string{"started", "outcome_unknown", "invalid"} {
		_, err = admin.Exec(`INSERT INTO `+app+`.rollout_effects(rollout,step,status) VALUES($1,'uncertain',$2)`, f.command.ID, status)
		require.NoError(t, err)
		requireCode(t, UpdatePostgresCatalogue(t.Context(), desired, oldDigest, f.config.Postgres.DSNFile), "catalogue_update_not_quiescent")
		_, err = admin.Exec(`DELETE FROM ` + app + `.rollout_effects WHERE step='uncertain'`)
		require.NoError(t, err)
	}
	_, err = admin.Exec(`ALTER TABLE ` + app + `.rollout_commands ADD COLUMN unknown_column TEXT`)
	require.NoError(t, err)
	requireCode(t, UpdatePostgresCatalogue(t.Context(), desired, oldDigest, f.config.Postgres.DSNFile), "postgres_storage_format_refused")
	var current string
	require.NoError(t, admin.QueryRow(`SELECT catalogue FROM `+app+`.rollout_storage WHERE id=1`).Scan(&current))
	require.Equal(t, oldDigest, current)
}

func TestPostgresCatalogueUpdatePreservesBackendDestinationDuringCredentialRefresh(t *testing.T) {
	f, _, _ := canonicalFixture(t, "plan", "opentofu")
	f.config.Executor.KubernetesBackendConfigPath = postgresBackendFile(t, "https://127.0.0.1:64431", "old-token")
	admin := openPostgresFixture(t, f)
	oldDigest := f.engine.ToolsDigest()
	desired := catalogueUpdateConfig(t, f.config)
	desired.Executor.KubernetesBackendConfigPath = postgresBackendFile(t, "https://127.0.0.1:64431", "new-token")
	closeFixture(t, f.engine)
	before := catalogueHistory(t, admin, f.config)
	wrong := catalogueUpdateConfig(t, desired)
	wrong.Executor.KubernetesBackendConfigPath = postgresBackendFile(t, "https://127.0.0.1:64432", "new-token")
	requireCode(t, UpdatePostgresCatalogue(t.Context(), wrong, oldDigest, f.config.Postgres.DSNFile), "native_backend_identity_conflict")
	require.Equal(t, before, catalogueHistory(t, admin, f.config))
	require.NoError(t, UpdatePostgresCatalogue(t.Context(), desired, oldDigest, f.config.Postgres.DSNFile))
	require.Equal(t, before, catalogueHistory(t, admin, f.config))
	f.config = desired
	var err error
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
}

func TestPostgresCatalogueUpdateNoopStillRequiresQuiescenceAndAuthority(t *testing.T) {
	f, _, _ := postgresFixture(t, "reviewed")
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	requireCode(t, UpdatePostgresCatalogue(t.Context(), f.config, f.engine.ToolsDigest(), f.config.Postgres.DSNFile), "catalogue_update_not_quiescent")
}

func TestPostgresCatalogueUpdateRefusesActualActivePlanWithoutEnvironmentOwner(t *testing.T) {
	f, _, _ := canonicalFixture(t, "plan", "opentofu")
	ready, release := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "release")
	body := fmt.Sprintf(`import json,sys,pathlib,time
r=json.load(sys.stdin)
pathlib.Path(%q).write_text('running')
while not pathlib.Path(%q).exists(): time.sleep(0.01)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))
`, ready, release)
	addTask(t, f, "pre_plan", "read_only", body)
	_ = openPostgresFixture(t, f)
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("continue"), 0600) })
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second, 10*time.Millisecond)
	var owners int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_environments`).Scan(&owners))
	require.Zero(t, owners, "Plan intentionally has no Environment ownership")
	requireCode(t, UpdatePostgresCatalogue(t.Context(), catalogueUpdateConfig(t, f.config), f.engine.ToolsDigest(), f.config.Postgres.DSNFile), "catalogue_update_not_quiescent")
	require.NoError(t, os.WriteFile(release, []byte("continue"), 0600))
	waitPhase(t, f.engine, f.command.ID, "completed")
}

func TestPostgresCatalogueUpdateChecksProgressBeyondFirstPage(t *testing.T) {
	f, trace, _ := postgresFixture(t, "plan")
	oldDigest := f.engine.ToolsDigest()
	closeFixture(t, f.engine)
	dsn, err := readPostgresDSN(f.config.Postgres)
	require.NoError(t, err)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	// Seed retained terminal records with workers stopped: the property under
	// test is the storage scan, not workflow admission or cancellation.
	for n := range 65 {
		command := f.command
		command.ID = fmt.Sprintf("page-%03d", n)
		command.Request.ID = command.ID
		payload, err := json.Marshal(command)
		require.NoError(t, err)
		encrypted, err := f.engine.encrypt(payload)
		require.NoError(t, err)
		progress := Progress{ID: command.ID, EnvironmentID: command.EnvironmentID, Phase: "cancelled", Revision: 1, CreatedAt: time.Now().UTC(), Steps: []StepResult{}}
		data, err := json.Marshal(progress)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO rollout_commands(id,digest,payload,progress,version,environment,strategy) VALUES($1,$2,$3,$4,$5,$6,$7)`, command.ID, digest(payload), encrypted, data, command.Version, command.EnvironmentID, command.Strategy)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM rollout_commands`).Scan(&count))
	require.Equal(t, 65, count)
	beforeNoop := catalogueHistory(t, db, f.config)
	generations := catalogueGenerations(t, db, f.config)
	require.NoError(t, UpdatePostgresCatalogue(t.Context(), f.config, oldDigest, f.config.Postgres.DSNFile))
	require.Equal(t, beforeNoop, catalogueHistory(t, db, f.config))
	require.Equal(t, generations, catalogueGenerations(t, db, f.config))
	var data []byte
	require.NoError(t, db.QueryRow(`SELECT progress FROM rollout_commands WHERE id='page-064'`).Scan(&data))
	var progress Progress
	require.NoError(t, json.Unmarshal(data, &progress))
	progress.Phase = "planning"
	data, err = json.Marshal(progress)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE rollout_commands SET progress=$1 WHERE id='page-064'`, data)
	require.NoError(t, err)
	beforeRefusal := catalogueHistory(t, db, f.config)
	requireCode(t, UpdatePostgresCatalogue(t.Context(), catalogueUpdateConfig(t, f.config), oldDigest, f.config.Postgres.DSNFile), "catalogue_update_not_quiescent")
	require.Equal(t, beforeRefusal, catalogueHistory(t, db, f.config))
	require.Equal(t, generations, catalogueGenerations(t, db, f.config))
	_, err = os.Stat(trace)
	require.True(t, err == nil || os.IsNotExist(err), "read execution trace: %v", err)
	if err == nil {
		require.Zero(t, operationCount(t, trace, "plan"))
		require.Zero(t, operationCount(t, trace, "apply"))
	}
}
