//go:build integration

package rollout

import (
	"bytes"
	"encoding/json"
	"testing"

	"filippo.io/age"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stretchr/testify/require"
)

func TestPostgresNativeProviderPreparationFailureKeepsProtectedDiagnostic(t *testing.T) {
	f, trace, _ := postgresFixture(t, "plan")
	managed, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	f.command.Request.EncryptedLogsRecipient = managed.Recipient().String()
	for _, name := range []string{"HOME", "USERPROFILE", "home", "TF_CLI_CONFIG_FILE", "TERRAFORM_CONFIG", "TF_REATTACH_PROVIDERS", "TOFU_REATTACH_PROVIDERS", "TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE", "TOFU_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE"} {
		t.Setenv(name, "")
	}
	_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	failed := waitPhase(t, f.engine, f.command.ID, "failed")
	require.Equal(t, "plan_failed", failed.ErrorCode)
	require.Empty(t, failed.Subject)
	require.False(t, failed.InfrastructureApplied)
	require.Equal(t, 1, operationCount(t, trace, "init"))
	require.Zero(t, operationCount(t, trace, "plan"))
	require.Zero(t, operationCount(t, trace, "apply"))
	logs, err := f.engine.GetPlanLogs(t.Context(), failed.ID, "")
	require.NoError(t, err)
	const diagnostic = "Retained provider preparation failed: "
	require.Contains(t, string(logs.Content), diagnostic)
	require.Contains(t, string(logs.Content), "is not defined")
	var stored []byte
	require.NoError(t, f.engine.db.QueryRow(`SELECT content FROM rollout_plan_logs WHERE rollout=$1`, failed.ID).Scan(&stored))
	require.True(t, bytes.HasPrefix(stored, []byte("age-encryption.org/v1")))
	require.NotContains(t, string(stored), diagnostic)
	require.NotNil(t, failed.LogCapture)
	delivery := nativeCaptureDelivery(t, f, failed.ID, failed.LogCapture.Authority.CaptureRevision)
	require.NotNil(t, delivery.Log)
	decoded, err := nativelogs.Decode(delivery.Log.Ciphertext, managed, delivery.Log.Descriptor.Authority)
	require.NoError(t, err)
	require.Equal(t, logs.Content, decoded.Logs)
	public, err := json.Marshal(failed)
	require.NoError(t, err)
	require.NotContains(t, string(public), diagnostic)
	require.NotContains(t, string(public), "is not defined")
	var effects, artifacts int
	require.NoError(t, f.engine.db.QueryRow(`SELECT (SELECT count(*) FROM rollout_effects WHERE rollout=$1),(SELECT count(*) FROM rollout_artifacts WHERE rollout=$1)`, failed.ID).Scan(&effects, &artifacts))
	require.Zero(t, effects)
	require.Zero(t, artifacts)
	reopenPostgres(t, f)
	restored, err := f.engine.GetPlanLogs(t.Context(), failed.ID, "")
	require.NoError(t, err)
	require.Equal(t, logs.Content, restored.Content)
	require.Zero(t, operationCount(t, trace, "plan"))
	require.Zero(t, operationCount(t, trace, "apply"))
}
