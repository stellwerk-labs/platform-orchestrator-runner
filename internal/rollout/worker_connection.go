package rollout

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type workerConnector struct {
	driver.Connector
	engine *Engine
}

func (c workerConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		c.engine.storageFailed(ctx)
		return nil, errStorageUnavailable
	}
	return conn, nil
}

func (e *Engine) openWorkerDatabase(dsn string) (*sql.DB, error) {
	poolConfig, err := durablePostgresPoolConfig(dsn)
	if err != nil {
		return nil, refused("postgres_configuration_refused")
	}
	config := poolConfig.ConnConfig
	config.Tracer = e
	config.RuntimeParams["application_name"] = "stellwerk-native:" + e.config.Postgres.ApplicationSchema + ":" + e.config.Postgres.WorkerID
	connector := stdlib.GetConnector(*config, stdlib.OptionAfterConnect(e.connectSlot))
	db := sql.OpenDB(workerConnector{connector, e})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func (e *Engine) connectSlot(ctx context.Context, conn *pgx.Conn) (result error) {
	defer func() {
		if result != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = conn.Close(closeCtx)
		}
	}()
	if err := verifyPostgresDurability(ctx, conn); err != nil {
		return err
	}
	var owned bool
	pg := e.config.Postgres
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, pg.ApplicationSchema+":slot:"+pg.WorkerID).Scan(&owned); err != nil || !owned {
		return errStorageUnavailable
	}
	if expected := e.generation.Load(); expected > 0 {
		var current int64
		if err := conn.QueryRow(ctx, `SELECT generation FROM rollout_workers WHERE worker=$1`, pg.WorkerID).Scan(&current); err != nil {
			return errStorageUnavailable
		}
		if current != expected {
			e.stopWorker(errGenerationSuperseded)
			return errGenerationSuperseded
		}
	}
	return nil
}
