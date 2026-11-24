package rollout

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/lib/pq"
)

// PostgresConfig is explicit: no environment DSN, generated key or SQLite fallback.
// Every worker needs its own private scratch Directory and the same retained key.
type PostgresConfig struct {
	Concurrency       int    `json:"concurrency"`
	WorkerConcurrency int    `json:"worker_concurrency"`
	DSNFile           string `json:"dsn_file"`
	IdentityFile      string `json:"identity_file"`
	WorkerID          string `json:"worker_id"`
	ApplicationSchema string `json:"application_schema"`
	SystemSchema      string `json:"system_schema"`
}

var postgresIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func validatePostgresConfig(pg *PostgresConfig) error {
	if pg == nil {
		return nil
	}
	if pg.WorkerConcurrency < 1 || pg.WorkerConcurrency > pg.Concurrency || pg.Concurrency > 1024 || !identifier.MatchString(pg.WorkerID) || !postgresIdentifier.MatchString(pg.ApplicationSchema) || !postgresIdentifier.MatchString(pg.SystemSchema) || pg.ApplicationSchema == pg.SystemSchema || strings.HasPrefix(pg.ApplicationSchema, "pg_") || strings.HasPrefix(pg.SystemSchema, "pg_") {
		return refused("postgres_configuration_refused")
	}
	return nil
}

// InitializePostgres initializes a fresh explicit profile without recovering work.
// Normal Open verifies only; an operator can use a separate DDL-capable role here.
func InitializePostgres(ctx context.Context, config Config) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	if config.Postgres == nil {
		return refused("postgres_configuration_refused")
	}
	directory, lock, err := privateDirectory(config.Directory)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	e := &Engine{config: config, executor: config.Executor}
	defer func() {
		if e.db != nil {
			_ = e.db.Close()
		}
	}()
	if err = e.openPostgres(ctx, directory, true); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err = dbos.Migrate(ctx, e.postgresDSN, dbos.WithMigrateSchema(config.Postgres.SystemSchema), dbos.WithMigrateLogger(logger)); err != nil {
		return errors.New("postgres_workflow_storage_unavailable")
	}
	client, err := dbos.NewClient(ctx, dbos.ClientConfig{AppName: "stellwerk-runner-rollouts", DatabaseURL: e.postgresDSN, DatabaseSchema: config.Postgres.SystemSchema, Logger: logger})
	if err != nil {
		return errStorageUnavailable
	}
	defer func() { _ = dbos.Shutdown(client, time.Second) }()
	if err = e.initializeQueue(client); err != nil {
		return err
	}
	if _, err = e.db.ExecContext(ctx, `UPDATE rollout_storage SET workflow_ready=1 WHERE id=1`); err != nil {
		return errors.New("postgres_migration_unavailable")
	}
	return nil
}

func (e *Engine) openPostgres(ctx context.Context, directory string, initialize bool) error {
	if err := refuseSQLiteHistory(directory); err != nil {
		return err
	}
	execution := *e.executor
	e.executor = &execution
	if err := e.executor.PrepareNativeBackend(); err != nil {
		return refused("native_backend_configuration_refused")
	}
	pg := e.config.Postgres
	identity, err := LoadStorageIdentity(pg.IdentityFile)
	if err != nil {
		return err
	}
	if err = e.executor.PrepareNativeCatalogue(identity); err != nil {
		return refused("storage_identity_unavailable")
	}
	dsn, err := readPostgresDSN(pg)
	if err != nil {
		return err
	}
	e.db, err = e.openWorkerDatabase(dsn)
	if err != nil {
		return err
	}
	e.postgresDSN = dsn
	e.executor.Identity, e.executor.Directory = identity, filepath.Join(directory, "work")
	if err = e.db.PingContext(ctx); err != nil {
		return err
	}
	if initialize {
		if err = e.initializePostgresSchema(ctx); err != nil {
			return err
		}
	}
	if err = e.verifyPostgresStorage(ctx, initialize); err != nil {
		return err
	}
	if !initialize {
		return e.activateSlot(ctx)
	}
	return nil
}

// LoadStorageIdentity preserves the single bare X25519 key policy used by both
// PostgreSQL runtime and pre-enrollment Native catalogue export.
func LoadStorageIdentity(path string) (*age.X25519Identity, error) {
	key, err := readStorageSecret(path, 1024)
	if err != nil {
		return nil, refused("storage_identity_unavailable")
	}
	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(key)))
	if err != nil {
		return nil, refused("storage_identity_unavailable")
	}
	return identity, nil
}

func refuseSQLiteHistory(directory string) error {
	for _, name := range []string{"rollouts.sqlite", "rollouts.sqlite-wal", "rollouts.sqlite-shm", "identity.agekey"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); !errors.Is(err, os.ErrNotExist) {
			return refused("postgres_requires_fresh_directory")
		}
	}
	return nil
}

func readStorageSecret(path string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("storage_secret_unavailable")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > limit || info.Mode().Perm()&0007 != 0 {
		return nil, errors.New("storage_secret_unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("storage_secret_unavailable")
	}
	return data, nil
}

func (e *Engine) initializePostgresSchema(ctx context.Context) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1937007986,2)`); err != nil {
		return errors.New("postgres_migration_unavailable")
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, e.config.Postgres.ApplicationSchema).Scan(&exists); err != nil {
		return errors.New("postgres_migration_unavailable")
	}
	if exists {
		return tx.Commit()
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, e.config.Postgres.SystemSchema).Scan(&exists); err != nil || exists {
		return refused("postgres_existing_workflow_storage_refused")
	}
	if _, err = tx.ExecContext(ctx, `CREATE SCHEMA `+pq.QuoteIdentifier(e.config.Postgres.ApplicationSchema)); err != nil {
		return errors.New("postgres_migration_unavailable")
	}
	if _, err = tx.ExecContext(ctx, postgresSchema); err != nil {
		return errors.New("postgres_migration_unavailable")
	}
	if err = e.insertPostgresIdentity(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) insertPostgresIdentity(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO rollout_site(id,org,runner,runner_uuid,public_key) VALUES(1,$1,$2,$3,$4)`, e.config.OrgID, e.config.RunnerID, e.config.RunnerUUID, digest(e.config.PublicKey))
	if err != nil {
		return errors.New("postgres_migration_unavailable")
	}
	check, err := e.encrypt(e.postgresIdentityCheck())
	if err != nil {
		return refused("storage_identity_unavailable")
	}
	structure, err := postgresStructure(ctx, tx)
	if err != nil {
		return refused("postgres_storage_format_refused")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO rollout_storage(id,version,identity_check,catalogue,application,system_schema,schema_digest,structure_digest) VALUES(1,$1,$2,$3,$4,$5,$6,$7)`, postgresStorageVersion, check, e.executor.CatalogDigest(), e.runtimeVersion(), e.config.Postgres.SystemSchema, digest([]byte(postgresSchema)), structure)
	if err != nil {
		return errors.New("postgres_migration_unavailable")
	}
	return nil
}

func (e *Engine) verifyPostgresStorage(ctx context.Context, initialize bool) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	check, structure, err := e.verifyPostgresBinding(ctx, tx, e.executor.CatalogDigest())
	if err != nil {
		return err
	}
	if !initialize {
		var ready bool
		if err = tx.QueryRowContext(ctx, `SELECT workflow_ready=1 FROM rollout_storage WHERE id=1`).Scan(&ready); err != nil || !ready {
			return refused("postgres_initialization_incomplete")
		}
	}
	plain, err := e.decrypt(check)
	if err != nil {
		return refused("storage_identity_unavailable")
	}
	if string(plain) != string(e.postgresIdentityCheck()) {
		return refused("native_backend_identity_conflict")
	}
	current, err := postgresStructure(ctx, tx)
	if err != nil || current != structure {
		return refused("postgres_storage_format_refused")
	}
	if err = e.checkStoredCommands(ctx, tx, false); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) postgresIdentityCheck() []byte {
	check, _ := json.Marshal(struct {
		Version int             `json:"version"`
		Purpose string          `json:"purpose"`
		Backend json.RawMessage `json:"kubernetes_backend"`
	}{1, storageIdentityCheck, e.executor.NativeBackendIdentity()})
	return check
}

// Bind actual columns, defaults and constraints, not just the claimed version.
// Runtime can verify this using catalog reads without DDL privileges.
func postgresStructure(ctx context.Context, tx *sql.Tx) (string, error) {
	var columns, constraints string
	err := tx.QueryRowContext(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_array(c.relname,a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,a.attidentity,pg_get_expr(d.adbin,d.adrelid)) ORDER BY c.relname,a.attnum)::text,'[]')
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
 LEFT JOIN pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum
 WHERE n.nspname=current_schema() AND c.relkind='r'`).Scan(&columns)
	if err != nil {
		return "", err
	}
	err = tx.QueryRowContext(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_array(c.relname,p.contype,pg_get_constraintdef(p.oid,true)) ORDER BY c.relname,p.conname)::text,'[]')
 FROM pg_constraint p JOIN pg_class c ON c.oid=p.conrelid
 JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema()`).Scan(&constraints)
	return digest([]byte(columns + "\n" + constraints)), err
}

func readPostgresDSN(pg *PostgresConfig) (string, error) {
	raw, err := readStorageSecret(pg.DSNFile, 8192)
	if err != nil || strings.TrimSpace(string(raw)) == "" {
		return "", refused("postgres_configuration_refused")
	}
	dsn := strings.TrimSpace(string(raw))
	return durablePostgresDSN(dsn, pg.ApplicationSchema)
}

func (e *Engine) verifyPostgresBinding(ctx context.Context, tx *sql.Tx, expectedCatalogue string) ([]byte, string, error) {
	var version int
	var org, site, siteUUID, trust, catalogue, application, systemSchema, schemaDigest, structure string
	var check []byte
	err := tx.QueryRowContext(ctx, `SELECT s.version,s.identity_check,s.catalogue,s.application,s.system_schema,s.schema_digest,s.structure_digest,i.org,i.runner,i.runner_uuid::text,i.public_key FROM rollout_storage s CROSS JOIN rollout_site i WHERE s.id=1 AND i.id=1`).Scan(&version, &check, &catalogue, &application, &systemSchema, &schemaDigest, &structure, &org, &site, &siteUUID, &trust)
	if err != nil || version != postgresStorageVersion || schemaDigest != digest([]byte(postgresSchema)) || application != e.runtimeVersion() || systemSchema != e.config.Postgres.SystemSchema {
		return nil, "", refused("postgres_storage_format_refused")
	}
	if org != e.config.OrgID || site != e.config.RunnerID || siteUUID != e.config.RunnerUUID || trust != digest(e.config.PublicKey) {
		return nil, "", refused("site_identity_conflict")
	}
	if catalogue != expectedCatalogue {
		return nil, "", refused("catalog_refused")
	}
	return check, structure, nil
}
