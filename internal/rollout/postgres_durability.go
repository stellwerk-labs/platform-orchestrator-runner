package rollout

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func durablePostgresDSN(dsn, schema string) (string, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil || config.RuntimeParams["options"] != "" {
		return "", refused("postgres_configuration_refused")
	}
	commit := strings.ToLower(config.RuntimeParams["synchronous_commit"])
	switch commit {
	case "", "on", "off", "local", "remote_write":
		commit = "on"
	case "remote_apply":
	default:
		return "", refused("postgres_configuration_refused")
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "", refused("postgres_configuration_refused")
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			return "", refused("postgres_configuration_refused")
		}
		for key, value := range map[string]string{"search_path": schema, "connect_timeout": "5", "synchronous_commit": commit, "target_session_attrs": "read-write"} {
			query.Set(key, value)
		}
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	}
	return "application_name=stellwerk-runner " + dsn + " search_path=" + schema + " connect_timeout=5 synchronous_commit=" + commit + " target_session_attrs=read-write", nil
}

func durablePostgresPoolConfig(dsn string) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, refused("postgres_configuration_refused")
	}
	return config, nil
}

func verifyPostgresDurability(ctx context.Context, conn *pgx.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var durable bool
	err := conn.QueryRow(ctx, `SELECT current_setting('synchronous_commit') IN ('on','remote_apply')
 AND current_setting('fsync')='on' AND current_setting('full_page_writes')='on'
 AND current_setting('synchronous_standby_names')<>''
 AND NOT pg_is_in_recovery() AND current_setting('transaction_read_only')='off'`).Scan(&durable)
	if err != nil || !durable {
		return refused("postgres_durability_refused")
	}
	return nil
}

func (e *Engine) newWorkflowPool(ctx context.Context) (*pgxpool.Pool, error) {
	config, err := durablePostgresPoolConfig(e.postgresDSN)
	if err != nil {
		return nil, err
	}
	parsed, err := pgx.ParseConfig(e.postgresDSN)
	if err != nil {
		return nil, refused("postgres_configuration_refused")
	}
	if parsed.RuntimeParams["pool_max_conns"] == "" {
		config.MaxConns = 20
	}
	config.MinConns, config.MaxConnLifetime, config.MaxConnIdleTime = 0, time.Hour, 5*time.Minute
	config.ConnConfig.ConnectTimeout = 10 * time.Second
	config.ConnConfig.RuntimeParams["application_name"] = "stellwerk-runner-rollouts"
	if config.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] == "" {
		config.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "60000"
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := verifyPostgresDurability(ctx, conn); err != nil {
			e.storageFailed(ctx)
			return err
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errStorageUnavailable
	}
	return pool, nil
}
