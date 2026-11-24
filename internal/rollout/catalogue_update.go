package rollout

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// UpdatePostgresCatalogue is an offline maintenance operation. Operators must
// stop admission and the supported worker cohort first, including offline carriers.
// It never launches DBOS, migrates schemas or rewrites retained execution history.
func UpdatePostgresCatalogue(ctx context.Context, config Config, expectedOld, adminDSNFile string) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	defer func() {
		var public *Error
		if resultErr != nil && !errors.As(resultErr, &public) {
			resultErr = refused("catalogue_update_unavailable")
		}
	}()
	if err := validateConfig(config); err != nil {
		return err
	}
	raw, err := hex.DecodeString(expectedOld)
	if config.Postgres == nil || adminDSNFile == "" || len(raw) != 32 || err != nil || expectedOld != strings.ToLower(expectedOld) {
		return refused("catalogue_update_configuration_refused")
	}
	directory, err := filepath.Abs(config.Directory)
	if err != nil {
		return err
	}
	if err = refuseSQLiteHistory(directory); err != nil {
		return err
	}
	execution := *config.Executor
	e := &Engine{config: config, executor: &execution}
	if err = e.executor.PrepareNativeBackend(); err != nil {
		return refused("native_backend_configuration_refused")
	}
	identity, err := LoadStorageIdentity(config.Postgres.IdentityFile)
	if err != nil {
		return err
	}
	if err = e.executor.PrepareNativeCatalogue(identity); err != nil {
		return refused("storage_identity_unavailable")
	}
	e.executor.Identity = identity
	if _, err = e.executor.PublicInventory(); err != nil {
		return refused("catalogue_update_configuration_refused")
	}
	pg := *config.Postgres
	pg.DSNFile = adminDSNFile
	dsn, err := readPostgresDSN(&pg)
	if err != nil {
		return err
	}
	poolConfig, err := durablePostgresPoolConfig(dsn)
	if err != nil {
		return err
	}
	e.db = stdlib.OpenDB(*poolConfig.ConnConfig, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		if err := verifyPostgresDurability(ctx, conn); err != nil {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
			defer closeCancel()
			_ = conn.Close(closeCtx)
			return err
		}
		return nil
	}))
	defer func() { _ = e.db.Close() }()
	e.db.SetMaxOpenConns(1)
	return e.updatePostgresCatalogue(ctx, expectedOld)
}

func (e *Engine) updatePostgresCatalogue(ctx context.Context, expectedOld string) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Reuse initializer serialization. No per-worker session locks are acquired.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1937007986,2)`); err != nil {
		return err
	}
	if err = e.authorizeCatalogueUpdate(ctx, tx); err != nil {
		return err
	}
	var current string
	if err = tx.QueryRowContext(ctx, `SELECT catalogue FROM rollout_storage WHERE id=1 FOR UPDATE`).Scan(&current); err != nil {
		return err
	}
	if err = lockCatalogueWorkers(ctx, tx); err != nil {
		return err
	}
	desired := e.executor.CatalogDigest()
	if current != expectedOld && current != desired {
		return refused("catalogue_update_conflict")
	}
	if err = e.verifyCatalogueUpdate(ctx, tx, current); err != nil {
		return err
	}
	if err = catalogueQuiescent(ctx, tx); err != nil {
		return err
	}
	// Retrying a committed update must not supersede the replacement cohort again.
	if current == desired {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE rollout_storage SET catalogue=$1 WHERE id=1`, desired); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE rollout_workers SET generation=generation+1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) authorizeCatalogueUpdate(ctx context.Context, tx *sql.Tx) error {
	var authorized bool
	err := tx.QueryRowContext(ctx, `SELECT count(*)=2 AND coalesce(bool_and(pg_has_role(current_user,nspowner,'USAGE')),false)
 FROM pg_namespace WHERE nspname IN ($1,$2)`, e.config.Postgres.ApplicationSchema, e.config.Postgres.SystemSchema).Scan(&authorized)
	if err != nil {
		return err
	}
	if !authorized {
		return refused("catalogue_update_admin_required")
	}
	return nil
}

func lockCatalogueWorkers(ctx context.Context, tx *sql.Tx) error {
	// Adoption may hold its own worker, then a command, then the prior worker.
	// NOWAIT avoids a maintenance/adoption cycle even with ordered acquisition.
	rows, err := tx.QueryContext(ctx, `SELECT worker FROM rollout_workers ORDER BY worker FOR UPDATE NOWAIT`)
	if err != nil {
		return refused("catalogue_update_busy")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var worker string
		if err = rows.Scan(&worker); err != nil {
			return refused("catalogue_update_busy")
		}
	}
	if err = rows.Err(); err != nil {
		return refused("catalogue_update_busy")
	}
	if err = rows.Close(); err != nil {
		return refused("catalogue_update_busy")
	}
	return nil
}

func (e *Engine) verifyCatalogueUpdate(ctx context.Context, tx *sql.Tx, current string) error {
	check, structure, err := e.verifyPostgresBinding(ctx, tx, current)
	if err != nil {
		return err
	}
	var ready bool
	if err = tx.QueryRowContext(ctx, `SELECT workflow_ready=1 FROM rollout_storage WHERE id=1`).Scan(&ready); err != nil || !ready {
		return refused("postgres_initialization_incomplete")
	}
	plain, err := e.decrypt(check)
	if err != nil {
		return refused("storage_identity_unavailable")
	}
	if !bytes.Equal(plain, e.postgresIdentityCheck()) {
		return refused("native_backend_identity_conflict")
	}
	actual, err := postgresStructure(ctx, tx)
	if err != nil || structure != actual {
		return refused("postgres_storage_format_refused")
	}
	return e.checkStoredCommands(ctx, tx, false)
}

func catalogueQuiescent(ctx context.Context, tx *sql.Tx) error {
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rollout_environments)
 OR EXISTS(SELECT 1 FROM rollout_effects WHERE status<>'done')`).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return refused("catalogue_update_not_quiescent")
	}
	// Plans deliberately need no Environment owner. Inspect every retained progress
	// page under worker exclusion instead of treating the owner table as a ledger.
	var previous string
	for {
		next, count, err := catalogueProgressPage(ctx, tx, previous)
		if err != nil {
			return err
		}
		if count < 64 {
			return nil
		}
		previous = next
	}
}

func catalogueProgressPage(ctx context.Context, tx *sql.Tx, previous string) (string, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.environment,c.strategy,CASE WHEN octet_length(c.progress)<=4194304 THEN c.progress END,
 EXISTS(SELECT 1 FROM rollout_effects e WHERE e.rollout=c.id AND e.step='apply' AND e.status='done')
 FROM rollout_commands c WHERE c.id>$1 ORDER BY c.id LIMIT 64`, previous)
	if err != nil {
		return previous, 0, err
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var id, environment, strategy string
		var data []byte
		var applied bool
		if err = rows.Scan(&id, &environment, &strategy, &data, &applied); err != nil {
			return previous, count, err
		}
		if !catalogueSafeProgress(id, environment, strategy, data, applied) {
			return previous, count, refused("catalogue_update_not_quiescent")
		}
		previous, count = id, count+1
	}
	if err = rows.Err(); err != nil {
		return previous, count, err
	}
	return previous, count, rows.Close()
}

func catalogueSafeProgress(id, environment, strategy string, data []byte, applied bool) bool {
	var p Progress
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil || decoder.Decode(new(any)) != io.EOF || p.ID != id || p.EnvironmentID != environment || p.Revision < 1 || p.CreatedAt.IsZero() || p.InfrastructureApplied != applied {
		return false
	}
	switch p.Phase {
	case "completed":
		if p.FailedStep != "" || p.ErrorCode != "" || !catalogueCompletedProgress(p, strategy) {
			return false
		}
	case "failed", "denied", "cancelled":
		if p.InfrastructureApplied {
			return false
		}
	default:
		return false
	}
	for _, step := range p.Steps {
		if step.ID == "" {
			return false
		}
		switch step.Status {
		case "ok":
		case "failed", "denied":
			if p.Phase == "completed" || step.Phase == "post_apply" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func catalogueCompletedProgress(p Progress, strategy string) bool {
	switch strategy {
	case "plan", "observation":
		return !p.InfrastructureApplied
	case "apply", "reviewed", "apply_saved", "destroy":
		return p.InfrastructureApplied
	default:
		return false
	}
}
