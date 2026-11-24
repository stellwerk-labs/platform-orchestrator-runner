//go:build integration

package rollout

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestPostgresDurabilityBoundsLiveWorkflowPoolAfterCallerTimeout(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	require.True(t, f.engine.Ready())
	config, err := pgx.ParseConfig(f.engine.postgresDSN)
	require.NoError(t, err)
	require.Nil(t, config.TLSConfig)
	require.True(t, net.ParseIP(config.Host).IsLoopback())

	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
	})
	address, proxy := startPostgresDurabilityBlackhole(t, net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))))
	endpoint := url.URL{Scheme: "postgresql", Host: address, User: url.UserPassword(config.User, config.Password), Path: "/" + config.Database}
	query := url.Values{"sslmode": {"disable"}, "target_session_attrs": {"read-write"}}
	for key, value := range config.RuntimeParams {
		query.Set(key, value)
	}
	endpoint.RawQuery = query.Encode()
	f.engine.postgresDSN = endpoint.String()
	pool, err = f.engine.newWorkflowPool(context.Background())
	require.NoError(t, err)
	require.Zero(t, pool.Config().MinConns)
	require.Zero(t, pool.Config().MinIdleConns)
	require.Zero(t, pool.Stat().TotalConns())

	caller, cancel := context.WithTimeout(context.Background(), time.Second)
	acquisition := make(chan error, 1)
	acquisitionDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-acquisitionDone:
		case <-time.After(2 * time.Second):
			t.Error("caller Acquire did not join")
		}
	})
	go func() {
		defer close(acquisitionDone)
		conn, acquireErr := pool.Acquire(caller)
		if conn != nil {
			conn.Release()
		}
		acquisition <- acquireErr
	}()

	var started time.Time
	select {
	case started = <-proxy.query:
	case <-time.After(5 * time.Second):
		t.Fatal("effective-settings SQL never reached the real PostgreSQL connection")
	}
	require.True(t, proxy.readyAtQuery.Load(), "real ReadyForQuery must precede the effective-settings SQL")
	select {
	case err = <-acquisition:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorIs(t, caller.Err(), context.DeadlineExceeded)
	case <-time.After(3 * time.Second):
		t.Fatal("caller Acquire did not expire independently")
	}
	// Acquire expiry leaves its detached constructor alive. Keep the pool open
	// until Fatal so only the constructor's own durability-check bound can win.
	require.True(t, f.engine.Ready())
	require.Equal(t, int32(1), pool.Stat().ConstructingConns())
	require.Greater(t, proxy.dropped.Load(), int64(0), "real PostgreSQL replies must have been discarded")
	select {
	case err = <-f.engine.Fatal():
		require.ErrorIs(t, err, errStorageUnavailable)
	case <-time.After(time.Until(started.Add(12 * time.Second))):
		t.Fatal("open-pool constructor did not reach its local durability-check deadline")
	}
	elapsed := time.Since(started)
	require.GreaterOrEqual(t, elapsed, 9*time.Second)
	require.Less(t, elapsed, 12*time.Second)
	require.False(t, f.engine.Ready())
	var effects int
	require.NoError(t, admin.QueryRowContext(t.Context(), "SELECT count(*) FROM "+pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)+".rollout_effects").Scan(&effects))
	require.Zero(t, effects)
}

type postgresDurabilityBlackhole struct {
	query        chan time.Time
	ready        atomic.Bool
	readyAtQuery atomic.Bool
	blocked      atomic.Bool
	dropped      atomic.Int64
}

func startPostgresDurabilityBlackhole(t *testing.T, target string) (string, *postgresDurabilityBlackhole) {
	t.Helper()
	server, err := net.DialTimeout("tcp", target, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &postgresDurabilityBlackhole{query: make(chan time.Time, 1)}
	accepted := make(chan net.Conn, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = listener.Close()
		_ = server.Close()
		select {
		case client := <-accepted:
			if client != nil {
				_ = client.Close()
			}
		case <-time.After(2 * time.Second):
			t.Error("PostgreSQL blackhole Accept did not join")
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("PostgreSQL blackhole transport did not join")
		}
	})
	go func() {
		defer close(done)
		client, acceptErr := listener.Accept()
		_ = listener.Close()
		if acceptErr != nil {
			close(accepted)
			return
		}
		// Publish before forwarding so cleanup can interrupt blocked writes too.
		accepted <- client
		close(accepted)
		forwarded := make(chan struct{})
		go func() {
			defer close(forwarded)
			defer server.Close()
			proxy.forwardClientToPostgres(client, server)
		}()
		proxy.forwardPostgresToClient(server, client)
		_ = client.Close()
		_ = server.Close()
		<-forwarded
	}()
	return listener.Addr().String(), proxy
}

func (p *postgresDurabilityBlackhole) forwardClientToPostgres(client, server net.Conn) {
	const token = "current_setting('synchronous_commit')"
	buffer := make([]byte, 8192)
	var tail []byte
	for {
		n, err := client.Read(buffer)
		if n > 0 {
			probe := append(tail, buffer[:n]...)
			if bytes.Contains(probe, []byte(token)) && !p.blocked.Load() {
				p.readyAtQuery.Store(p.ready.Load())
				p.blocked.Store(true)
				p.query <- time.Now()
			}
			tail = append([]byte(nil), probe[max(0, len(probe)-len(token)+1):]...)
			if _, writeErr := io.Copy(server, bytes.NewReader(buffer[:n])); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *postgresDurabilityBlackhole) forwardPostgresToClient(server, client net.Conn) {
	buffer := make([]byte, 8192)
	var tail []byte
	for {
		n, err := server.Read(buffer)
		if n > 0 {
			probe := append(tail, buffer[:n]...)
			if bytes.Contains(probe, []byte{'Z', 0, 0, 0, 5, 'I'}) {
				p.ready.Store(true)
			}
			tail = append([]byte(nil), probe[max(0, len(probe)-5):]...)
			if p.blocked.Load() {
				p.dropped.Add(int64(n))
			} else if _, writeErr := io.Copy(client, bytes.NewReader(buffer[:n])); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
