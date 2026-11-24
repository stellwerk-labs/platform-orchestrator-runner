package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func TestNativeStopPreservesDeadlineAndHTTPFailures(t *testing.T) {
	for _, reason := range []string{"cancelled", "deadline", "http_failure"} {
		t.Run(reason, func(t *testing.T) {
			public, _, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			engine, err := rollout.Open(t.Context(), rollout.Config{Directory: filepath.Join(t.TempDir(), "native"), OrgID: "org", RunnerID: "site", PublicKey: public, Executor: &rolloutexec.Executor{}})
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(t, engine.Close(ctx))
			})
			server := &http.Server{Addr: "127.0.0.1:0", Handler: http.NewServeMux(), ReadHeaderTimeout: time.Second}
			ctx, cancel := context.WithCancel(t.Context())
			switch reason {
			case "cancelled":
				cancel()
			case "deadline":
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			case "http_failure":
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				defer func() { require.NoError(t, listener.Close()) }()
				server.Addr = listener.Addr().String()
			}
			defer cancel()
			err = serveRollout(ctx, engine, server)
			if reason == "cancelled" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if reason == "deadline" {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}
			require.False(t, engine.Ready())
		})
	}
}

func TestNativeStartupRetainsCancelledHost(t *testing.T) {
	root := t.TempDir()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	publicFile, agentFile, tokenFile := filepath.Join(root, "command.pub"), filepath.Join(root, "agent.key"), filepath.Join(root, "operator.token")
	require.NoError(t, os.WriteFile(publicFile, []byte(base64.StdEncoding.EncodeToString(public)), 0600))
	require.NoError(t, os.WriteFile(agentFile, []byte("not-reached-before-engine-startup"), 0600))
	require.NoError(t, os.WriteFile(tokenFile, make([]byte, 32), 0600))
	body, err := json.Marshal(rolloutConfig{Directory: filepath.Join(root, "native"), OrgID: "org", RunnerID: "site", PublicKeyFile: publicFile, AgentPrivateKeyFile: agentFile, APITokenFile: tokenFile, Listen: "127.0.0.1:0"})
	require.NoError(t, err)
	file := filepath.Join(root, "config.json")
	require.NoError(t, os.WriteFile(file, body, 0600))
	t.Setenv("ROLLOUT_CONFIG_FILE", file)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = runRollout(ctx)
	require.ErrorIs(t, err, context.Canceled, "already-cancelled startup must not become an uncancelable runtime")
}
