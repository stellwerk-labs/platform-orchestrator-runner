//go:build integration && (linux || darwin)

package rolloutagent

import (
	"bytes"
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

// This gate seeds with a real retained format6 executable, not a marker edit in
// new storage. Its local backend is standalone Engine custody, not Core authority.
func TestNativeFormatSevenRefusesActualFormatSixHistory(t *testing.T) {
	oldBinary := os.Getenv("NATIVE_FORMAT6_RUNNER_BINARY")
	require.True(t, filepath.IsAbs(oldBinary), "explicit retained format6 binary is required")
	f := newHAProcessFixture(t)
	newBinary := f.binary
	f.binary = oldBinary
	gate := f.gateApply(t, "after")
	first := f.start(t, "worker-0", true)
	var version int
	require.NoError(t, f.db.QueryRow(`SELECT version FROM `+f.table("rollout_storage")).Scan(&version))
	require.Equal(t, 6, version, "the old executable did not initialize an actual format6 profile")
	awaiting := f.command(t, "reviewed")
	awaiting.NativeTarget, awaiting.ExecutionLimits = nil, nil
	awaiting.Request.DeploymentToken = "PRIVATE_OLD_FORMAT6_TOKEN"
	f.publish(t, awaiting)
	f.progress(t, awaiting.ID, "awaiting_approval")
	unknown := f.command(t, "apply")
	unknown.NativeTarget, unknown.ExecutionLimits = nil, nil
	unknown.Request.DeploymentToken = "PRIVATE_OLD_FORMAT6_TOKEN"
	f.publish(t, unknown)
	gate.entered(t)
	f.stateMarker(t, unknown.ID)
	require.Equal(t, 1, f.count(t, "rollout_effects", unknown.ID))
	first.kill(t)
	gate.killOrphan(t)
	// The actual Apply completed, but the killed old worker never acknowledged its claim.
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM `+f.table("rollout_effects")+` WHERE rollout=$1 AND step='apply'`, unknown.ID).Scan(&status))
	require.Equal(t, "started", status)
	key, err := os.ReadFile(f.pg.IdentityFile)
	require.NoError(t, err)
	before := f.nativeHistorySnapshot(t)
	evidenceRoot := os.Getenv("NATIVE_COMMAND_EVIDENCE_DIRECTORY")
	require.True(t, filepath.IsAbs(evidenceRoot), "explicit private evidence directory is required")
	evidence, err := os.MkdirTemp(evidenceRoot, "format6-")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(evidence, "retained.agekey"), key, 0400))
	snapshot, err := json.Marshal(before)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(evidence, "before.json"), snapshot, 0600))
	f.binary = newBinary
	failed := f.start(t, "worker-format7", false)
	select {
	case err := <-failed.done:
		failed.exited = true
		require.Error(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("new runtime did not refuse the old profile")
	}
	output, err := os.ReadFile(failed.log)
	require.NoError(t, err)
	require.Contains(t, string(output), "postgres_storage_format_refused")
	require.False(t, strings.Contains(string(output), string(key)), "startup exposed an old private key")
	require.NotContains(t, string(output), "PRIVATE_OLD_FORMAT6_TOKEN")
	require.NotContains(t, string(output), f.pg.IdentityFile)
	config, _ := f.workerConfig(t, "initializer-format7", f.pg.DSNFile)
	initialize := exec.CommandContext(t.Context(), newBinary, "rollout-storage-init")
	initialize.Env = append(os.Environ(), "ROLLOUT_CONFIG_FILE="+config)
	output, err = initialize.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "postgres_storage_format_refused")
	require.False(t, strings.Contains(string(output), string(key)), "initializer exposed an old private key")
	require.NotContains(t, string(output), f.pg.IdentityFile)
	after := f.nativeHistorySnapshot(t)
	snapshot, err = json.Marshal(after)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(evidence, "after.json"), snapshot, 0600))
	require.Equal(t, before, after, "refusal rewrote old custody or started recovery")
	retained, err := os.ReadFile(f.pg.IdentityFile)
	require.NoError(t, err)
	require.True(t, bytes.Equal(key, retained), "refusal changed the old private key")
	require.Equal(t, 1, f.count(t, "rollout_effects", unknown.ID))
	f.progress(t, awaiting.ID, "awaiting_approval")
}

func (f *haProcessFixture) nativeHistorySnapshot(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.db.Query(`SELECT table_schema,table_name FROM information_schema.tables WHERE table_schema IN ($1,$2) AND table_type='BASE TABLE' ORDER BY table_schema,table_name`, f.pg.ApplicationSchema, f.pg.SystemSchema)
	require.NoError(t, err)
	var tables [][2]string
	for rows.Next() {
		var schema, name string
		require.NoError(t, rows.Scan(&schema, &name))
		tables = append(tables, [2]string{schema, name})
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	snapshot := make(map[string]string, len(tables))
	for _, table := range tables {
		var data string
		query := `SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text),'[]'::jsonb)::text FROM ` + pq.QuoteIdentifier(table[0]) + "." + pq.QuoteIdentifier(table[1]) + ` r`
		require.NoError(t, f.db.QueryRow(query).Scan(&data))
		snapshot[table[0]+"."+table[1]] = data
	}
	return snapshot
}
