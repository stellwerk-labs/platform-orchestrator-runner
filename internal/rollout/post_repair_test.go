package rollout

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func TestPostRepairMetadataAcceptsOnlyPublicExactFields(t *testing.T) {
	good := `{"control_id":"11111111-1111-4111-8111-111111111111","expires_at":"2026-11-21T12:00:00Z"}`
	var authority PostRepairAuthority
	require.NoError(t, json.Unmarshal([]byte(good), &authority))
	for _, raw := range []string{
		`null`, `{}`, `{"control_id":"wrong","expires_at":"2026-11-21T12:00:00Z"}`,
		`{"control_id":"11111111-1111-4111-8111-111111111111","expires_at":null}`,
		`{"control_id":"11111111-1111-4111-8111-111111111111","expires_at":"0001-01-01T00:00:00Z"}`,
		`{"control_id":"11111111-1111-4111-8111-111111111111","control_id":"11111111-1111-4111-8111-111111111111","expires_at":"2026-11-21T12:00:00Z"}`,
		`{"CONTROL_ID":"11111111-1111-4111-8111-111111111111","expires_at":"2026-11-21T12:00:00Z"}`,
		`{"control_id expires_at":"11111111-1111-4111-8111-111111111111","expires_at":"2026-11-21T12:00:00Z"}`,
		`{"control_id":"11111111-1111-4111-8111-111111111111","expires_at":"2026-11-21T12:00:00Z","signed_payload":"PRIVATE"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var value PostRepairAuthority
			require.Error(t, json.Unmarshal([]byte(raw), &value))
		})
	}
	encoded, err := json.Marshal(authority)
	require.NoError(t, err)
	require.JSONEq(t, good, string(encoded))
}

func TestPostRepairDeadlineDoesNotRenewOtherStageAuthority(t *testing.T) {
	now := time.Now()
	command := Command{ExpiresAt: now.Add(-time.Second), ExecutionLimits: &ExecutionLimits{PlanSeconds: 1, ApplySeconds: 1, TaskSeconds: 10}}
	p := Progress{PostRepair: &PostRepairAuthority{ControlID: "11111111-1111-4111-8111-111111111111", ExpiresAt: now.Add(5 * time.Second)}}
	deadline, err := postDeadline(command, p, now)
	require.NoError(t, err)
	require.Equal(t, p.PostRepair.ExpiresAt, deadline)
	for _, stage := range []string{"plan", "observation", "apply", "direct-apply", "saved-apply", "preparation", "checks", "approval-ready", "plan-complete", "saved-ready", "observation-complete"} {
		_, err = nativeStageDeadline(command, stage, now)
		requireCode(t, err, "command_expired")
	}
	p.PostRepair.ExpiresAt = now.Add(time.Hour)
	deadline, err = postDeadline(command, p, now)
	require.NoError(t, err)
	require.Equal(t, now.Add(10*time.Second), deadline)
	command.ExpiresAt = now.Add(time.Hour)
	p.PostRepair.ExpiresAt = now
	_, err = postDeadline(command, p, now)
	requireCode(t, err, "post_repair_expired")
	p.PostRepair = nil
	command.ExpiresAt = now
	_, err = postDeadline(command, p, now)
	requireCode(t, err, "post_authority_expired")
}

func TestPostRepairDeadlineRetainsInterruptionAndUnsafeClassification(t *testing.T) {
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, retry := range []string{"receipt", "read_only"} {
		err := postTaskDeadline(deadline, rolloutexec.Tool{Retry: retry}, "post")
		var failure *operationDeadline
		require.ErrorAs(t, err, &failure)
		require.Equal(t, "post", failure.step)
		require.Equal(t, retry, failure.retry)
	}
	require.ErrorIs(t, postTaskDeadline(deadline, rolloutexec.Tool{Retry: "unsafe"}, "post"), context.DeadlineExceeded)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, postTaskDeadline(canceled, rolloutexec.Tool{Retry: "receipt"}, "post"), context.Canceled)
}
