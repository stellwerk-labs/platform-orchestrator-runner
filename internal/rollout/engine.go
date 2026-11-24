package rollout

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	_ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

// RuntimeVersion identifies the retained workflow and storage contract.
const RuntimeVersion = "stellwerk.runner.rollout/v1"
const NativeRuntimeVersion = "stellwerk.runner.rollout/v2"

var identifier = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type Engine struct {
	db           *sql.DB
	durable      dbos.Context
	lock         *os.File
	config       Config
	executor     *rolloutexec.Executor
	closed       atomic.Bool
	postgresDSN  string
	generation   atomic.Int64
	running      atomic.Bool
	stopped      atomic.Bool
	failure      chan error
	stageMu      sync.Mutex
	draining     bool
	activeStages int
}

func Open(ctx context.Context, config Config) (_ *Engine, resultErr error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	directory, lock, err := privateDirectory(config.Directory)
	if err != nil {
		return nil, err
	}
	e := &Engine{lock: lock, config: config, executor: config.Executor, failure: make(chan error, 1)}
	e.config.PublicKey = append(ed25519.PublicKey{}, config.PublicKey...)
	if config.Postgres != nil {
		pg := *config.Postgres
		e.config.Postgres = &pg
	}
	defer func() {
		if resultErr != nil {
			if e.durable != nil {
				_ = dbos.Shutdown(e.durable, time.Second)
			}
			if e.db != nil {
				_ = e.db.Close()
			}
			_ = lock.Close()
		}
	}()
	if err = e.openStorage(ctx, directory); err != nil {
		return nil, err
	}
	if err = e.initializeWorkflows(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func validateConfig(config Config) error {
	if !identifier.MatchString(config.OrgID) || !identifier.MatchString(config.RunnerID) || len(config.PublicKey) != ed25519.PublicKeySize || config.Executor == nil {
		return refused("invalid_configuration")
	}
	for _, name := range []string{"DBOS__APPVERSION", "DBOS__VMID", "DBOS__APPID", "DBOS__CLOUD", "DBOS__CONDUCTOR_KEY", "DBOS__CONDUCTOR_URL"} {
		if os.Getenv(name) != "" {
			return refused("dbos_environment_override")
		}
	}
	if _, err := RuntimeForProfile(config.Postgres != nil, config.RunnerUUID); err != nil {
		return err
	}
	return validatePostgresConfig(config.Postgres)
}

func (e *Engine) openStorage(ctx context.Context, directory string) error {
	if e.config.Postgres != nil {
		return e.openPostgres(ctx, directory, false)
	}
	identity, err := siteIdentity(directory)
	if err != nil {
		return err
	}
	e.executor.Identity = identity
	e.executor.Directory = filepath.Join(directory, "work")
	dbpath := filepath.Join(directory, "rollouts.sqlite")
	f, err := privateFile(dbpath)
	if err != nil {
		return err
	}
	_ = f.Close()
	e.db, err = sql.Open("sqlite", "file:"+dbpath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=secure_delete(ON)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return err
	}
	e.db.SetMaxOpenConns(1)
	if _, err = e.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	if err = e.bindSite(ctx); err != nil {
		return err
	}
	if err = e.initializeCommandStorage(ctx); err != nil {
		return err
	}
	return e.initializeNoEffectHistory(ctx)
}

func (e *Engine) initializeWorkflows(ctx context.Context) error {
	var err error
	durableConfig := dbos.Config{
		AppName: "stellwerk-runner-rollouts", SQLiteSystemDB: e.db,
		ApplicationVersion: e.runtimeVersion(), ExecutorID: e.config.OrgID + ":" + e.config.RunnerID,
		SystemDBStartupTimeout: 15 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if pg := e.config.Postgres; pg != nil {
		durableConfig.SQLiteSystemDB = nil
		pool, poolErr := e.newWorkflowPool(ctx)
		if poolErr != nil {
			return poolErr
		}
		defer func() {
			if e.durable == nil {
				pool.Close()
			}
		}()
		durableConfig.SystemDBPool, durableConfig.DatabaseSchema = pool, pg.SystemSchema
		durableConfig.ExecutorID = e.config.OrgID + ":" + e.config.RunnerUUID + ":" + pg.WorkerID
		durableConfig.SkipMigrations = true
	}
	e.durable, err = dbos.NewContext(context.Background(), durableConfig)
	if err != nil {
		if e.config.Postgres != nil {
			return errors.New("postgres_workflow_storage_unavailable")
		}
		return err
	}
	if e.config.Postgres != nil {
		if err = e.verifyQueue(e.durable); err != nil {
			return err
		}
	}
	if e.config.Postgres != nil {
		dbos.RegisterWorkflow(e.durable, controlReceipt, dbos.WithWorkflowName(controlReceiptWorkflow), dbos.WithMaxRecoveryAttempts(20))
	}
	dbos.RegisterWorkflow(e.durable, e.execute, dbos.WithWorkflowName(e.runtimeVersion()), dbos.WithMaxRecoveryAttempts(20))
	e.running.Store(true)
	if err = dbos.Launch(e.durable); err != nil {
		if e.config.Postgres != nil {
			return errors.New("postgres_workflow_storage_unavailable")
		}
		return err
	}
	if e.config.Postgres != nil {
		return nil
	}
	return e.resumeSQLiteImports(ctx)
}

func (e *Engine) resumeSQLiteImports(ctx context.Context) error {
	rows, err := e.List(ctx)
	if err != nil {
		return err
	}
	for _, p := range rows {
		if !terminal(p.Phase) {
			if err = e.start(p.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ToolsDigest reports the actual owned execution profile, including Native keying.
func (e *Engine) ToolsDigest() string { return e.executor.CatalogDigest() }

func (e *Engine) Close(ctx context.Context) error {
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		err := dbos.Shutdown(e.durable, time.Second)
		if err == nil {
			_ = e.db.Close()
			err = e.lock.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Retain the process lock until shutdown completes or the process exits.
		return refused("shutdown_incomplete")
	}
}

func (e *Engine) validate(command Command) error {
	if err := e.scope(command); err != nil {
		return err
	}
	if !validLifetime(command, time.Now()) {
		return refused("command_expired")
	}
	return e.validatePolicy(command)
}

func (e *Engine) validatePolicy(command Command) error {
	if len(command.Steps) > 32 || len(command.RequiredCapabilities) > 64 || command.ToolsDigest == "" || command.ToolsDigest != e.executor.CatalogDigest() {
		return refused("catalog_refused")
	}
	for _, capability := range command.RequiredCapabilities {
		if !slices.Contains(e.executor.Capabilities, capability) {
			return refused("capability_refused")
		}
	}
	a := command.Approval
	if (command.Version == Version || command.Strategy == "reviewed" || command.Recipe != nil && a.Mode != "") && !validApproval(a) {
		return refused("approval_policy_refused")
	}
	if err := e.validateRecipe(command); err != nil {
		return err
	}
	if err := e.validateCost(command); err != nil {
		return err
	}
	if err := e.validateStrategy(command); err != nil {
		return err
	}
	if e.executor.Validate(command.Request, command.Steps) != nil {
		return refused("steps_refused")
	}
	return nil
}

func (e *Engine) scope(command Command) error {
	legacy := e.config.Postgres == nil && command.Version == Version
	if (!legacy && command.Version != e.pipelineVersion()) || !identifier.MatchString(command.ID) || command.OrgID != e.config.OrgID || command.RunnerID != e.config.RunnerID || command.RunnerUUID != e.config.RunnerUUID || !identifier.MatchString(command.EnvironmentID) {
		return refused("command_scope_refused")
	}
	if command.Request.ID != command.ID || command.Request.OrgID != command.OrgID || command.Request.RunnerID != command.RunnerID || command.Request.RunnerUUID != command.RunnerUUID || command.Request.EnvironmentID != command.EnvironmentID {
		return refused("request_scope_refused")
	}
	return e.nativeCommandScope(command)
}

func (e *Engine) Submit(ctx context.Context, signed SignedCommand) (Progress, error) {
	if err := e.checkAdmission(); err != nil {
		return Progress{}, err
	}
	command, err := e.verify(signed)
	if err != nil {
		return Progress{}, err
	}
	p, err := e.importCommand(ctx, command, signed.Payload)
	if e.stopped.Load() {
		return Progress{}, errStorageUnavailable
	}
	if err != nil {
		return Progress{}, err
	}
	if e.config.Postgres == nil && !terminal(p.Phase) {
		err = e.start(p.ID)
	}
	return p, err
}

func (e *Engine) verify(signed SignedCommand) (Command, error) {
	if len(signed.Payload) == 0 || len(signed.Payload) > 4<<20 {
		return Command{}, refused("command_size_refused")
	}
	signature, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil || !ed25519.Verify(e.config.PublicKey, signed.Payload, signature) {
		return Command{}, refused("signature_refused")
	}
	return e.decodeCommandPayload(signed.Payload)
}

func (e *Engine) importCommand(ctx context.Context, command Command, payload []byte) (Progress, error) {
	tx, err := e.beginCommand(ctx)
	if err != nil {
		return Progress{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = e.lockImport(ctx, tx, command); err != nil {
		return Progress{}, err
	}
	var existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT digest FROM rollout_commands WHERE id=$1`, command.ID).Scan(&existingDigest)
	if err == nil {
		return existingImport(ctx, tx, command.ID, existingDigest, payload)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Progress{}, err
	}
	if err = e.validate(command); err != nil {
		return Progress{}, err
	}
	if command.SourcePlan != nil {
		if _, err = e.savedSource(ctx, tx, command); err != nil {
			return Progress{}, err
		}
	}
	return e.acceptCommand(ctx, tx, command, payload)
}

func (e *Engine) acceptCommand(ctx context.Context, tx *sql.Tx, command Command, payload []byte) (Progress, error) {
	encrypted, err := e.encrypt(payload)
	if err != nil {
		return Progress{}, err
	}
	p := Progress{ID: command.ID, EnvironmentID: command.EnvironmentID, Phase: "accepted", CreatedAt: time.Now().UTC(), Steps: []StepResult{}}
	if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_commands(id,digest,payload,progress,version,environment,strategy) VALUES($1,$2,$3,$4,$5,$6,$7)`, command.ID, digest(payload), encrypted, "{}", command.Version, command.EnvironmentID, command.Strategy); err != nil {
		return Progress{}, err
	}
	if command.Recipe != nil && command.SourcePlan != nil {
		if err = e.inheritSavedReview(ctx, tx, command, &p); err != nil {
			return Progress{}, err
		}
	}
	if err = consumeSourcePlan(ctx, tx, command); err != nil {
		return Progress{}, err
	}
	if command.Strategy != "plan" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_environments(environment,rollout) VALUES($1,$2)`, command.EnvironmentID, command.ID); err != nil {
			return Progress{}, refused("environment_busy")
		}
	}
	if err = saveProgress(ctx, tx, &p); err != nil {
		return Progress{}, err
	}
	if e.config.Postgres != nil {
		client, cancel := e.transactionClient(ctx)
		defer cancel()
		if _, err = dbos.Enqueue[reference](client, rolloutQueue, e.runtimeVersion(), command.ID, dbos.WithEnqueueWorkflowID(command.ID), dbos.WithEnqueueApplicationVersion(e.runtimeVersion()), dbos.WithEnqueueTransaction(tx)); err != nil {
			e.storageFailed(ctx)
			return Progress{}, errStorageUnavailable
		}
	}
	return p, tx.Commit()
}

func existingImport(ctx context.Context, tx *sql.Tx, id, expected string, payload []byte) (Progress, error) {
	if expected != digest(payload) {
		return Progress{}, refused("command_id_conflict")
	}
	p, err := readProgress(ctx, tx, id)
	if err != nil {
		return Progress{}, err
	}
	return p, tx.Commit()
}

func (e *Engine) start(id string) error {
	if _, err := dbos.RunWorkflow(e.durable, e.execute, id, dbos.WithWorkflowID(id)); err != nil {
		return errors.New("workflow_start_pending")
	}
	return nil
}

func (e *Engine) List(ctx context.Context) ([]Progress, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT c.progress FROM rollout_commands c JOIN rollout_events e ON e.rollout=c.id AND e.revision=1 ORDER BY e.id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]Progress, 0)
	for rows.Next() {
		var data []byte
		var p Progress
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (e *Engine) Get(ctx context.Context, id string) (Progress, error) {
	return readProgress(ctx, e.db, id)
}

func terminal(phase string) bool {
	return slices.Contains([]string{"completed", "failed", "denied", "cancelled", "outcome_unknown"}, phase)
}

func (e *Engine) finish(ctx context.Context, id, phase, code string) error {
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := readProgress(ctx, tx, id)
	if err != nil {
		return err
	}
	if terminal(p.Phase) {
		return nil
	}
	var unfinished int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM rollout_effects WHERE rollout=$1 AND status='started'`, id).Scan(&unfinished); err != nil {
		return err
	}
	if (unfinished > 0 || p.InfrastructureApplied) && phase != "completed" {
		phase = "outcome_unknown"
	}
	p.Phase, p.ErrorCode = phase, code
	if phase == "completed" {
		p.FailedStep = ""
	}
	if err = saveProgress(ctx, tx, &p); err != nil {
		return err
	}
	if phase != "outcome_unknown" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM rollout_environments WHERE rollout=$1`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func auditActor(actor string) bool {
	return strings.TrimSpace(actor) != "" && len(actor) <= 128
}

func (e *Engine) bindSite(ctx context.Context) error {
	if _, err := e.db.ExecContext(ctx, `INSERT INTO rollout_site(id,org,runner,public_key) VALUES(1,$1,$2,$3) ON CONFLICT(id) DO NOTHING`, e.config.OrgID, e.config.RunnerID, digest(e.config.PublicKey)); err != nil {
		return err
	}
	var org, runner, key string
	if err := e.db.QueryRowContext(ctx, `SELECT org,runner,public_key FROM rollout_site WHERE id=1`).Scan(&org, &runner, &key); err != nil {
		return err
	}
	if org != e.config.OrgID || runner != e.config.RunnerID || key != digest(e.config.PublicKey) {
		return refused("site_identity_conflict")
	}
	return nil
}

func validApproval(a Approval) bool {
	valid := (a.Mode == "local" || a.Mode == "policy" || a.Mode == "cost_non_increasing") && a.MaxAdd >= 0 && a.MaxChange >= 0 && a.MaxDestroy >= 0
	if a.PolicyID != "" || a.PolicyVersion != "" {
		valid = valid && identifier.MatchString(a.PolicyID) && identifier.MatchString(a.PolicyVersion)
	}
	if a.Mode == "cost_non_increasing" {
		valid = valid && a.PolicyID != "" && a.PolicyVersion != ""
	}
	return valid
}

func validLifetime(command Command, now time.Time) bool {
	return !command.IssuedAt.After(now.Add(time.Minute)) && command.ExpiresAt.After(now) && command.ExpiresAt.After(command.IssuedAt) && command.ExpiresAt.Sub(command.IssuedAt) <= 7*24*time.Hour
}

// RuntimeForProfile selects the wire/runtime contract without opening storage.
func RuntimeForProfile(postgres bool, runnerUUID string) (string, error) {
	if !rolloutexec.ValidRunnerUUID(runnerUUID) || postgres != (runnerUUID != "") {
		return "", refused("runner_uuid_refused")
	}
	if postgres {
		return NativeRuntimeVersion, nil
	}
	return RuntimeVersion, nil
}

func (e *Engine) runtimeVersion() string {
	if e.config.Postgres != nil {
		return NativeRuntimeVersion
	}
	return RuntimeVersion
}

func (e *Engine) pipelineVersion() string {
	if e.config.Postgres != nil {
		return NativePipelineVersion
	}
	return PipelineVersion
}
