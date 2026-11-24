package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stretchr/testify/require"
)

func TestRolloutWorkerIdentityBinding(t *testing.T) {
	for _, tc := range []struct {
		name, stored, environment string
		postgres, valid           bool
	}{
		{"absent_sqlite", "", "", false, true},
		{"absent_postgres", "static-slot", "", true, true},
		{"bind_slot", "", "runner-0", true, true},
		{"same_static", "runner-0", "runner-0", true, true},
		{"valid_characters", "", "Aa_0.:-", true, true},
		{"maximum", "", strings.Repeat("x", 128), true, true},
		{"sqlite_refused", "", "PRIVATE_WORKER_CANARY", false, false},
		{"conflict", "other", "PRIVATE_WORKER_CANARY", true, false},
		{"whitespace", "", " runner-0", true, false},
		{"slash", "", "runner/0", true, false},
		{"newline", "", "runner-0\n", true, false},
		{"unicode", "", "rúnner", true, false},
		{"too_long", "", strings.Repeat("x", 129), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := rolloutConfig{}
			if tc.postgres {
				cfg.Postgres = &rollout.PostgresConfig{WorkerID: tc.stored}
			}
			err := bindRolloutWorkerID(&cfg, tc.environment)
			if !tc.valid {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "PRIVATE_WORKER_CANARY")
				if tc.postgres {
					require.Equal(t, tc.stored, cfg.Postgres.WorkerID)
				}
				return
			}
			require.NoError(t, err)
			if tc.postgres {
				expected := tc.stored
				if tc.environment != "" {
					expected = tc.environment
				}
				require.Equal(t, expected, cfg.Postgres.WorkerID)
			}
		})
	}
}

func TestLoadRolloutConfigBindsBeforeTrustReads(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "verification-key")
	require.NoError(t, os.WriteFile(public, []byte(base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))), 0600))
	cfg := rolloutConfig{Postgres: &rollout.PostgresConfig{}, Listen: "127.0.0.1:8081", PublicKeyFile: public, Directory: filepath.Join(root, "unused-storage")}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	file := filepath.Join(root, "rollout.json")
	require.NoError(t, os.WriteFile(file, raw, 0600))
	t.Setenv("ROLLOUT_CONFIG_FILE", file)
	t.Setenv("ROLLOUT_WORKER_ID", "runner-0")
	actual, _, err := loadRolloutConfig()
	require.NoError(t, err)
	require.Equal(t, "runner-0", actual.Postgres.WorkerID)
	require.NoError(t, os.Remove(public))
	t.Setenv("ROLLOUT_WORKER_ID", "PRIVATE_WORKER_CANARY/invalid")
	_, _, err = loadRolloutConfig()
	require.EqualError(t, err, "invalid rollout worker identity binding")
	_, err = os.Stat(cfg.Directory)
	require.True(t, os.IsNotExist(err))
}
