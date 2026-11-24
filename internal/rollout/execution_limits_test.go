package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func TestNativeExecutionLimitsRequireCompleteBoundedExactMembers(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"plan_seconds":1,"apply_seconds":1}`,
		`{"plan_seconds":null,"apply_seconds":1,"task_seconds":1}`,
		`{"plan_seconds":0,"apply_seconds":1,"task_seconds":1}`,
		`{"plan_seconds":1,"apply_seconds":-1,"task_seconds":1}`,
		`{"plan_seconds":14401,"apply_seconds":1,"task_seconds":1}`,
		`{"plan_seconds":1,"apply_seconds":86401,"task_seconds":1}`,
		`{"plan_seconds":1,"apply_seconds":1,"task_seconds":14401}`,
		`{"plan_seconds":9223372036854775808,"apply_seconds":1,"task_seconds":1}`,
		`{"plan_seconds":1.5,"apply_seconds":1,"task_seconds":1}`,
		`{"plan_seconds":1,"apply_seconds":1,"task_seconds":1,"other":1}`,
		`{"PLAN_SECONDS":1,"apply_seconds":1,"task_seconds":1}`,
		`{"plan_seconds apply_seconds":1,"apply_seconds":1,"task_seconds":1}`,
		`{"plan_seconds":1,"plan_seconds":2,"apply_seconds":1,"task_seconds":1}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var limits ExecutionLimits
			require.Error(t, json.Unmarshal([]byte(raw), &limits))
		})
	}
	for _, raw := range []string{
		`{"task_seconds":1,"plan_seconds":1,"apply_seconds":1}`,
		`{"plan_seconds":14400,"apply_seconds":86400,"task_seconds":14400}`,
	} {
		var limits ExecutionLimits
		require.NoError(t, json.Unmarshal([]byte(raw), &limits))
		require.True(t, limits.valid())
	}
}

func TestNativeStageDeadlinesSelectAllExistingStagesAndIntersectExpiry(t *testing.T) {
	now := time.Now()
	command := Command{ExpiresAt: now.Add(time.Hour), ExecutionLimits: &ExecutionLimits{PlanSeconds: 303, ApplySeconds: 701, TaskSeconds: 503}}
	for _, test := range []struct {
		names   string
		seconds int64
	}{
		{"plan observation", 303},
		{"apply direct-apply saved-apply", 701},
		{"preparation checks post-tasks", 503},
		{"approval-ready plan-complete saved-ready observation-complete", 120},
	} {
		for _, name := range strings.Fields(test.names) {
			t.Run(name, func(t *testing.T) {
				deadline, err := nativeStageDeadline(command, name, now)
				require.NoError(t, err)
				require.Equal(t, time.Duration(test.seconds)*time.Second, deadline.Sub(now))
				nearExpiry := command
				nearExpiry.ExpiresAt = now.Add(100 * time.Millisecond)
				deadline, err = nativeStageDeadline(nearExpiry, name, now)
				require.NoError(t, err)
				require.Equal(t, nearExpiry.ExpiresAt, deadline)
			})
		}
	}
	_, err := nativeStageDeadline(command, "future-effect", now)
	requireCode(t, err, "execution_limits_refused")
	command.ExpiresAt = now
	_, err = nativeStageDeadline(command, "apply", now)
	requireCode(t, err, "command_expired")
	command.ExecutionLimits = nil
	_, err = nativeStageDeadline(command, "apply", now)
	requireCode(t, err, "execution_limits_refused")
}

func TestNativeApplyDeadlineHandlesSuccessfulExpiredReturnsWithoutPrivateErrors(t *testing.T) {
	native := &Engine{config: Config{Postgres: &PostgresConfig{}}}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for _, test := range []struct {
		name string
		out  rolloutexec.Outcome
		err  error
		code string
	}{
		{"success", rolloutexec.Outcome{Started: true, Applied: true}, nil, "apply_outcome_unknown"},
		{"success-before-start-marker", rolloutexec.Outcome{}, nil, "apply_outcome_unknown"},
		{"started-error", rolloutexec.Outcome{Started: true}, errors.New("process failed"), "apply_outcome_unknown"},
		{"before-effect-error", rolloutexec.Outcome{}, errors.New("prepare failed"), "execution_prepare_failed"},
		{"claim-refusal", rolloutexec.Outcome{}, refused("apply_outcome_unknown"), "apply_outcome_unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := []byte("PRIVATE_DEADLINE_LOG_CANARY")
			test.out.Logs = original
			failure := native.applyDeadline(ctx, test.out, test.err, "execution_prepare_failed")
			require.NotNil(t, failure)
			require.Equal(t, test.code, failure.code)
			require.Equal(t, original, failure.logs)
			original[0] = 'x'
			require.Equal(t, byte('P'), failure.logs[0], "captured diagnostics cannot alias a mutable executor buffer")
			require.NotContains(t, failure.Error(), "PRIVATE_DEADLINE_LOG_CANARY")
			require.NotContains(t, fmt.Sprintf("%+v", failure), "PRIVATE_DEADLINE_LOG_CANARY")
			encoded, err := json.Marshal(map[string]any{"failure": failure})
			require.NoError(t, err)
			require.JSONEq(t, `{"failure":{}}`, string(encoded), "the transient value has no exported checkpoint fields")
		})
	}
	tooLarge := bytes.Repeat([]byte("x"), rolloutexec.PlanLogLimit+100)
	failure := native.applyDeadline(ctx, rolloutexec.Outcome{Started: true, Logs: tooLarge}, nil, "execution_prepare_failed")
	require.Len(t, failure.logs, rolloutexec.PlanLogLimit+1, "preserve finite oversize detection within a bounded private value")
	require.Nil(t, native.applyDeadline(context.Background(), rolloutexec.Outcome{Started: true, Applied: true}, nil, "execution_prepare_failed"))
	legacy := &Engine{}
	require.Nil(t, legacy.applyDeadline(ctx, rolloutexec.Outcome{Started: true}, errors.New("expired legacy process"), "execution_prepare_failed"))
}
