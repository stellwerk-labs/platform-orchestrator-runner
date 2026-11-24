//go:build integration

package rollout

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestPostgresDurabilityNormalizesBothDSNForms(t *testing.T) {
	raw, err := readStorageSecret(os.Getenv("RUNNER_POSTGRES_TEST_DSN_FILE"), 8192)
	require.NoError(t, err)
	config, err := pgx.ParseConfig(string(raw))
	require.NoError(t, err)
	address := url.URL{Scheme: "postgres", Host: net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), User: url.UserPassword(config.User, config.Password), Path: "/" + config.Database}
	for _, uri := range []bool{false, true} {
		for _, commit := range []string{"off", "remote_apply"} {
			t.Run(fmt.Sprintf("uri_%t_%s", uri, commit), func(t *testing.T) {
				dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable synchronous_commit=%s", config.Host, config.Port, config.User, config.Password, config.Database, commit)
				if uri {
					address.RawQuery = url.Values{"sslmode": {"disable"}, "synchronous_commit": {commit}}.Encode()
					dsn = address.String()
				}
				path := filepath.Join(t.TempDir(), "postgres.dsn")
				require.NoError(t, os.WriteFile(path, []byte(dsn), 0600))
				normalized, err := readPostgresDSN(&PostgresConfig{DSNFile: path, ApplicationSchema: "public"})
				require.NoError(t, err)
				conn, err := pgx.Connect(t.Context(), normalized)
				require.NoError(t, err)
				defer func() { require.NoError(t, conn.Close(t.Context())) }()
				var actual string
				require.NoError(t, conn.QueryRow(t.Context(), "SHOW synchronous_commit").Scan(&actual))
				want := "on"
				if commit == "remote_apply" {
					want = commit
				}
				require.Equal(t, want, actual)
				require.NoError(t, verifyPostgresDurability(t.Context(), conn))
				if uri {
					query := address.Query()
					query.Set("options", "-c synchronous_commit=off")
					address.RawQuery = query.Encode()
					dsn = address.String()
				} else {
					dsn += " options='-c synchronous_commit=off'"
				}
				require.NoError(t, os.WriteFile(path, []byte(dsn), 0600))
				_, err = readPostgresDSN(&PostgresConfig{DSNFile: path, ApplicationSchema: "public"})
				requireCode(t, err, "postgres_configuration_refused")
			})
		}
	}
}

func TestPostgresDurabilityRefusesUnsafeSettingsAndStandby(t *testing.T) {
	require.Equal(t, "1", os.Getenv("AGENT_DELIVERY_ISOLATED"), "requires the invocation-owned HA PostgreSQL")
	raw, err := readStorageSecret(os.Getenv("RUNNER_POSTGRES_TEST_DSN_FILE"), 8192)
	require.NoError(t, err)
	conn, err := pgx.Connect(t.Context(), string(raw))
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close(t.Context())) }()
	f, _, admin := postgresFixture(t, "plan")
	closeFixture(t, f.engine)
	refuseOpen := func(t *testing.T, dsnFile string) {
		query := "SELECT generation FROM " + pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema) + ".rollout_workers WHERE worker='worker-0'"
		var before, after int64
		require.NoError(t, admin.QueryRow(query).Scan(&before))
		config, pg := f.config, *f.config.Postgres
		config.Directory = filepath.Join(t.TempDir(), "refused")
		pg.DSNFile, config.Postgres = dsnFile, &pg
		engine, openErr := Open(t.Context(), config)
		require.Error(t, openErr)
		require.Nil(t, engine)
		require.NoError(t, admin.QueryRow(query).Scan(&after))
		require.Equal(t, before, after)
	}
	require.NoError(t, verifyPostgresDurability(t.Context(), conn))
	_, err = conn.Exec(t.Context(), "SET synchronous_commit=off")
	require.NoError(t, err)
	requireCode(t, verifyPostgresDurability(t.Context(), conn), "postgres_durability_refused")
	_, err = conn.Exec(t.Context(), "SET synchronous_commit=on")
	require.NoError(t, err)
	for _, setting := range []string{"full_page_writes", "fsync", "synchronous_standby_names"} {
		t.Run(setting, func(t *testing.T) {
			var original string
			require.NoError(t, conn.QueryRow(t.Context(), "SELECT current_setting($1)", setting).Scan(&original))
			defer func() {
				_, restoreErr := conn.Exec(t.Context(), "ALTER SYSTEM SET "+setting+" = "+pq.QuoteLiteral(original))
				require.NoError(t, restoreErr)
				_, restoreErr = conn.Exec(t.Context(), "SELECT pg_reload_conf()")
				require.NoError(t, restoreErr)
				require.Eventually(t, func() bool { return verifyPostgresDurability(t.Context(), conn) == nil }, 5*time.Second, 20*time.Millisecond)
			}()
			value := "off"
			if setting == "synchronous_standby_names" {
				value = ""
			}
			_, err := conn.Exec(t.Context(), "ALTER SYSTEM SET "+setting+" = "+pq.QuoteLiteral(value))
			require.NoError(t, err)
			_, err = conn.Exec(t.Context(), "SELECT pg_reload_conf()")
			require.NoError(t, err)
			require.Eventually(t, func() bool { return verifyPostgresDurability(t.Context(), conn) != nil }, 5*time.Second, 20*time.Millisecond)
			refuseOpen(t, os.Getenv("RUNNER_POSTGRES_TEST_DSN_FILE"))
			before := catalogueHistory(t, admin, f.config)
			requireCode(t, UpdatePostgresCatalogue(t.Context(), f.config, f.engine.ToolsDigest(), f.config.Postgres.DSNFile), "postgres_durability_refused")
			require.Equal(t, before, catalogueHistory(t, admin, f.config))
		})
	}
	standby, err := readStorageSecret(os.Getenv("RUNNER_POSTGRES_STANDBY_TEST_DSN_FILE"), 8192)
	require.NoError(t, err)
	readOnly, err := pgx.Connect(t.Context(), string(standby))
	require.NoError(t, err)
	defer func() { require.NoError(t, readOnly.Close(t.Context())) }()
	requireCode(t, verifyPostgresDurability(t.Context(), readOnly), "postgres_durability_refused")
	refuseOpen(t, os.Getenv("RUNNER_POSTGRES_STANDBY_TEST_DSN_FILE"))
	before := catalogueHistory(t, admin, f.config)
	requireCode(t, UpdatePostgresCatalogue(t.Context(), f.config, f.engine.ToolsDigest(), os.Getenv("RUNNER_POSTGRES_STANDBY_TEST_DSN_FILE")), "catalogue_update_unavailable")
	require.Equal(t, before, catalogueHistory(t, admin, f.config))
}

func TestPostgresDurabilityRechecksNativeAndWorkflowReconnect(t *testing.T) {
	require.Equal(t, "1", os.Getenv("AGENT_DELIVERY_ISOLATED"), "requires the invocation-owned HA PostgreSQL")
	f, _, admin := postgresFixture(t, "plan")
	pool, err := f.engine.newWorkflowPool(t.Context())
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, pool.Ping(t.Context()))
	pool.Reset()
	var role string
	require.NoError(t, admin.QueryRow("SELECT current_user").Scan(&role))
	_, err = admin.Exec("ALTER ROLE " + pq.QuoteIdentifier(role) + " SET default_transaction_read_only=on")
	require.NoError(t, err)
	defer func() {
		_, resetErr := admin.Exec("ALTER ROLE " + pq.QuoteIdentifier(role) + " RESET default_transaction_read_only")
		require.NoError(t, resetErr)
	}()
	_, err = admin.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name=$1", "stellwerk-native:"+f.config.Postgres.ApplicationSchema+":worker-0")
	require.NoError(t, err)
	f.engine.db.SetMaxIdleConns(0)
	nativeCtx, nativeCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer nativeCancel()
	require.ErrorIs(t, f.engine.db.PingContext(nativeCtx), errStorageUnavailable)
	require.NoError(t, nativeCtx.Err())
	require.False(t, f.engine.Ready())
	workflowCtx, workflowCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer workflowCancel()
	require.Error(t, pool.Ping(workflowCtx))
	require.NoError(t, workflowCtx.Err())
	var effects int
	require.NoError(t, admin.QueryRow("SELECT count(*) FROM "+pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)+".rollout_effects").Scan(&effects))
	require.Zero(t, effects)
}
