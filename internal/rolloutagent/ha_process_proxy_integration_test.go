//go:build integration && (linux || darwin)

package rolloutagent

import (
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

type haPostgresProxy struct {
	listener net.Listener
	target   string
	mutex    sync.Mutex
	blocked  bool
	pairs    map[*haProxyPair]struct{}
}

type haProxyPair struct{ client, server net.Conn }

func (f *haProcessFixture) postgresProxy(t *testing.T) (*haPostgresProxy, string) {
	t.Helper()
	raw, err := os.ReadFile(f.pg.DSNFile)
	require.NoError(t, err)
	config, err := pgx.ParseConfig(string(raw))
	require.NoError(t, err)
	require.Nil(t, config.TLSConfig, "this disposable wire-fault gate requires its configured non-TLS local PostgreSQL")
	require.True(t, net.ParseIP(config.Host).IsLoopback(), "fault proxy must target the explicitly owned loopback fixture")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &haPostgresProxy{listener: listener, target: net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), pairs: make(map[*haProxyPair]struct{})}
	t.Cleanup(func() {
		proxy.partition()
		require.NoError(t, listener.Close())
	})
	go proxy.accept()
	address := &url.URL{Scheme: "postgresql", Host: listener.Addr().String(), User: url.UserPassword(config.User, config.Password), Path: "/" + config.Database}
	query := url.Values{"sslmode": {"disable"}, "connect_timeout": {"3"}}
	for key, value := range config.RuntimeParams {
		query.Set(key, value)
	}
	address.RawQuery = query.Encode()
	path := filepath.Join(f.root, "old-worker-proxy.dsn")
	require.NoError(t, os.WriteFile(path, []byte(address.String()), 0600))
	return proxy, path
}

func (proxy *haPostgresProxy) accept() {
	for {
		client, err := proxy.listener.Accept()
		if err != nil {
			return
		}
		go proxy.forward(client)
	}
}

func (proxy *haPostgresProxy) forward(client net.Conn) {
	proxy.mutex.Lock()
	blocked := proxy.blocked
	proxy.mutex.Unlock()
	if blocked {
		_ = client.Close()
		return
	}
	server, err := net.DialTimeout("tcp", proxy.target, time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	pair := &haProxyPair{client: client, server: server}
	proxy.mutex.Lock()
	if proxy.blocked {
		proxy.mutex.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	proxy.pairs[pair] = struct{}{}
	proxy.mutex.Unlock()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(server, client)
		_ = server.Close()
		close(done)
	}()
	_, _ = io.Copy(client, server)
	_ = client.Close()
	<-done
	proxy.mutex.Lock()
	delete(proxy.pairs, pair)
	proxy.mutex.Unlock()
}

func (proxy *haPostgresProxy) partition() {
	proxy.mutex.Lock()
	defer proxy.mutex.Unlock()
	proxy.blocked = true
	for pair := range proxy.pairs {
		_ = pair.client.Close()
		_ = pair.server.Close()
	}
}

func (proxy *haPostgresProxy) connectionCount() int {
	proxy.mutex.Lock()
	defer proxy.mutex.Unlock()
	return len(proxy.pairs)
}
