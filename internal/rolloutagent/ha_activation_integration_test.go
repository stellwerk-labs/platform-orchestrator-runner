//go:build integration && (linux || darwin)

package rolloutagent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestHAActivationRefusesValidCredentialsBeforeLaunch(t *testing.T) {
	f := newHAProcessFixture(t)
	f.initialize(t)
	for _, trial := range []struct {
		name, field string
		value       any
	}{
		{"missing", "", ""}, {"revoked", "state", "revoked"}, {"quarantined", "state", "quarantined"},
		{"unreconciled", "reconciliation", "unreconciled"}, {"wrong-site", "runner_uuid", "00000000-0000-4000-8000-000000000000"},
		{"wrong-dsn", "dsn_sha256", strings.Repeat("0", 64)}, {"wrong-key", "identity_sha256", strings.Repeat("0", 64)},
		{"wrong-catalogue", "catalogue_sha256", strings.Repeat("0", 64)}, {"wrong-runtime", "runtime_version", "old"},
		{"wrong-org", "org_id", "other"}, {"wrong-schema", "application_schema", "other"},
		{"unknown-version", "version", 2}, {"invalid-id", "activation_id", "not-an-id"},
	} {
		t.Run(trial.name, func(t *testing.T) {
			config, _ := f.workerConfig(t, "refused", f.pg.DSNFile)
			raw, err := os.ReadFile(config)
			require.NoError(t, err)
			var profile map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &profile))
			var activation string
			require.NoError(t, json.Unmarshal(profile["activation_file"], &activation))
			body, err := os.ReadFile(activation)
			require.NoError(t, err)
			if trial.field == "" {
				require.NoError(t, os.Remove(activation))
			} else {
				var grant map[string]any
				require.NoError(t, json.Unmarshal(body, &grant))
				grant[trial.field] = trial.value
				body, err = json.Marshal(grant)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(activation+".replacement", body, 0400))
				require.NoError(t, os.Rename(activation+".replacement", activation))
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			process := exec.CommandContext(ctx, f.binary, "rollout")
			process.Env = append(os.Environ(), "ROLLOUT_CONFIG_FILE="+config)
			output, err := process.CombinedOutput()
			require.Error(t, err)
			require.NoError(t, ctx.Err(), "activation refusal must precede recovery")
			require.Contains(t, string(output), "native_activation_refused")
			_, err = os.Stat(filepath.Join(filepath.Dir(config), "coordinator.lock"))
			require.True(t, os.IsNotExist(err), "refusal must precede Engine.Open")
			var rows int
			query := `SELECT (SELECT count(*) FROM ` + f.table("rollout_workers") + `)+(SELECT count(*) FROM ` + f.table("rollout_effects") + `)+(SELECT count(*) FROM ` + f.table("rollout_pipeline_results") + `)+` +
				`(SELECT count(*) FROM ` + f.table("rollout_deliveries") + `)+(SELECT count(*) FROM ` + f.table("rollout_commands") + `)+(SELECT count(*) FROM ` + pq.QuoteIdentifier(f.pg.SystemSchema) + `.workflow_status)`
			require.NoError(t, f.db.QueryRow(query).Scan(&rows))
			require.Zero(t, rows, "refusal must not launch, import, execute or deliver")
		})
	}
	worker := f.start(t, "worker-0", true)
	worker.kill(t)
	activation := f.activation(t, f.pg)
	body, err := os.ReadFile(activation)
	require.NoError(t, err)
	var grant map[string]any
	require.NoError(t, json.Unmarshal(body, &grant))
	grant["reconciliation"] = "reconciled"
	body, err = json.Marshal(grant)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(activation+".reconciled", body, 0400))
	f.config["activation_file"] = activation + ".reconciled"
	worker = f.start(t, "worker-1", true)
	worker.kill(t)
}
