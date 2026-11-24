package rollout

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stretchr/testify/require"
)

func TestNoEffectCompletionBindsActualPreApplyFailures(t *testing.T) {
	for _, reason := range []string{"plan", "check", "deny", "cancel"} {
		t.Run(reason, func(t *testing.T) {
			f, trace, _ := canonicalFixture(t, "reviewed", "opentofu")
			f.command.Request.DeploymentToken = "PRIVATE_RECEIPT_TOKEN_CANARY"
			switch reason {
			case "plan":
				f.command.Strategy, f.command.Request.Mode, f.command.Approval = "plan", "plan_only", Approval{}
				f.command.Request.Bundle = executionBundle(t, map[string]string{"main.tf": "PRIVATE_INVALID_PLAN invalid configuration"})
				f.command.Request.BundleSHA256 = digest(f.command.Request.Bundle)
			case "check":
				addTask(t, f, "plan_check", "read_only", `import json,sys
r=json.load(sys.stdin)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'deny','category':'policy'}))`)
			}
			signed := f.signed(t, f.command)
			_, err := f.engine.Submit(t.Context(), signed)
			require.NoError(t, err)
			var p Progress
			if reason == "plan" || reason == "check" {
				p = waitPhase(t, f.engine, f.command.ID, "failed")
			} else {
				waiting := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
				if reason == "deny" {
					p, err = f.engine.Decide(t.Context(), waiting.ID, waiting.Subject, "deny", "operator")
				} else {
					p, err = f.engine.Cancel(t.Context(), waiting.ID, "operator")
				}
				require.NoError(t, err)
			}
			body := assertNoEffectCompletion(t, f, p, signed.Payload)
			require.Zero(t, operationCount(t, trace, "apply"))
			closeFixture(t, f.engine)
			f.engine, err = Open(t.Context(), f.config)
			require.NoError(t, err)
			_, err = f.engine.Submit(t.Context(), signed)
			require.NoError(t, err)
			recovered, err := f.engine.Get(t.Context(), p.ID)
			require.NoError(t, err)
			require.Equal(t, body, assertNoEffectCompletion(t, f, recovered, signed.Payload))
		})
	}
}

func assertNoEffectCompletion(t *testing.T, f *fixture, p Progress, payload []byte) []byte {
	t.Helper()
	token, body, ready, err := f.engine.Completion(t.Context(), p)
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, f.command.Request.DeploymentToken, token)
	var result api.DeploymentResultsUpdateBody
	require.NoError(t, json.Unmarshal(body, &result))
	require.Equal(t, api.Failure, result.Status)
	require.NotNil(t, result.NoEffectReceipt)
	require.Equal(t, "stellwerk.no-effect/v1", string(result.NoEffectReceipt.Version))
	require.Equal(t, "not_started", string(result.NoEffectReceipt.Effect))
	require.Equal(t, digest(payload), result.NoEffectReceipt.CommandSha256)
	var ciphertext []byte
	require.NoError(t, f.engine.db.QueryRow(`SELECT payload FROM rollout_commands WHERE id=?`, p.ID).Scan(&ciphertext))
	require.NotEqual(t, digest(ciphertext), result.NoEffectReceipt.CommandSha256)
	require.Nil(t, result.Outputs)
	require.Empty(t, result.Metadata)
	require.Nil(t, result.ObservationResult)
	require.Equal(t, api.DeploymentTFResourceCounts{}, result.TfResourceCounts)
	require.NotContains(t, string(body), "PRIVATE_")
	return body
}

func TestCompletionRefusesCallerTerminalFlagsAndStaleRevisions(t *testing.T) {
	f, _, _ := canonicalFixture(t, "reviewed", "opentofu")
	signed := f.signed(t, f.command)
	accepted, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
	require.NoError(t, err)
	forged := accepted
	forged.Phase, forged.InfrastructureApplied = "failed", false
	token, body, ready, err := f.engine.Completion(t.Context(), forged)
	require.NoError(t, err)
	require.False(t, ready)
	require.Empty(t, token)
	require.Empty(t, body)
	cancelled, err := f.engine.Cancel(t.Context(), accepted.ID, "operator")
	require.NoError(t, err)
	forged.Phase = "cancelled"
	token, body, ready, err = f.engine.Completion(t.Context(), forged)
	require.NoError(t, err)
	require.False(t, ready)
	require.Empty(t, token)
	require.Empty(t, body)
	assertNoEffectCompletion(t, f, cancelled, signed.Payload)
	forged.ID = "missing-command"
	token, body, ready, err = f.engine.Completion(t.Context(), forged)
	require.Error(t, err)
	require.False(t, ready)
	require.Empty(t, token)
	require.Empty(t, body)
}

func TestNoEffectCompletionRefusesEveryClaimStatus(t *testing.T) {
	for _, status := range []string{"started", "done"} {
		t.Run(status, func(t *testing.T) {
			f, _, _ := canonicalFixture(t, "reviewed", "opentofu")
			signed := f.signed(t, f.command)
			_, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
			require.NoError(t, err)
			claim, err := f.engine.claim(t.Context(), f.command.ID, "task:external", false)
			require.NoError(t, err)
			require.Equal(t, "new", claim)
			var blocked bool
			require.NoError(t, f.engine.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM rollout_no_effect_blocks WHERE rollout=?)`, f.command.ID).Scan(&blocked))
			require.True(t, blocked, "the new task claim lost its durable fact")
			// Owned old-writer fixture: an effect row can predate the sticky fact.
			_, err = f.engine.db.Exec(`DELETE FROM rollout_no_effect_blocks WHERE rollout=?`, f.command.ID)
			require.NoError(t, err)
			if status == "done" {
				err = f.engine.recordStep(t.Context(), f.command.ID, StepResult{ID: "external", Phase: "post_apply", Status: "ok"})
				require.NoError(t, err)
			}
			require.NoError(t, f.engine.finish(t.Context(), f.command.ID, "failed", "task_failed"))
			p, err := f.engine.Get(t.Context(), f.command.ID)
			require.NoError(t, err)
			require.False(t, p.InfrastructureApplied)
			_, body, ready, err := f.engine.Completion(t.Context(), p)
			require.NoError(t, err)
			if ready {
				var result api.DeploymentResultsUpdateBody
				require.NoError(t, json.Unmarshal(body, &result))
				require.Nil(t, result.NoEffectReceipt)
			} else {
				require.Equal(t, "outcome_unknown", p.Phase)
			}
		})
	}
}

func TestNoEffectHistorySurvivesUnknownThenFailedAndUpgrade(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("upgrade=%t", upgrade), func(t *testing.T) {
			f, _, _ := canonicalFixture(t, "reviewed", "opentofu")
			addTask(t, f, "plan_check", "read_only", `import json,sys
r=json.load(sys.stdin)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'fault','category':'unknown_outcome'}))`)
			_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
			require.NoError(t, err)
			waitPhase(t, f.engine, f.command.ID, "outcome_unknown")
			// A later failure status cannot erase an earlier durable uncertainty.
			failed, err := f.engine.update(t.Context(), f.command.ID, func(p *Progress) error {
				p.Phase, p.ErrorCode = "failed", "task_failed"
				return nil
			})
			require.NoError(t, err)
			if upgrade {
				closeFixture(t, f.engine)
				editClosedCommandStorage(t, f, func(db *sql.DB) {
					_, err := db.Exec(`DROP TABLE rollout_no_effect_blocks`)
					require.NoError(t, err)
				})
				f.engine, err = Open(t.Context(), f.config)
				require.NoError(t, err)
			}
			_, body, ready, err := f.engine.Completion(t.Context(), failed)
			require.NoError(t, err)
			require.True(t, ready)
			var result api.DeploymentResultsUpdateBody
			require.NoError(t, json.Unmarshal(body, &result))
			require.Equal(t, api.Failure, result.Status)
			require.Nil(t, result.NoEffectReceipt)
			var claims int
			require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects WHERE rollout=?`, failed.ID).Scan(&claims))
			require.Zero(t, claims, "the read-only fault must exercise sticky history without a claim")
			var blocked bool
			require.NoError(t, f.engine.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM rollout_no_effect_blocks WHERE rollout=?)`, failed.ID).Scan(&blocked))
			require.True(t, blocked)
			if upgrade {
				// Owned GC fixture: the sticky fact must outlive detailed events.
				_, err = f.engine.db.Exec(`DELETE FROM rollout_events WHERE rollout=? AND json_extract(payload,'$.phase')='outcome_unknown'`, failed.ID)
				require.NoError(t, err)
				closeFixture(t, f.engine)
				f.engine, err = Open(t.Context(), f.config)
				require.NoError(t, err)
				_, replay, ready, err := f.engine.Completion(t.Context(), failed)
				require.NoError(t, err)
				require.True(t, ready)
				require.Equal(t, body, replay)
			}
		})
	}
}

func TestNoEffectHistoryUpgradeIsAtomic(t *testing.T) {
	f, _, _ := canonicalFixture(t, "reviewed", "opentofu")
	signed := f.signed(t, f.command)
	_, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
	require.NoError(t, err)
	closeFixture(t, f.engine)
	editClosedCommandStorage(t, f, func(db *sql.DB) {
		_, err := db.Exec(`DROP TABLE rollout_no_effect_blocks; UPDATE rollout_events SET payload='invalid JSON'`)
		require.NoError(t, err)
	})
	_, err = Open(t.Context(), f.config)
	require.Error(t, err)
	editClosedCommandStorage(t, f, func(db *sql.DB) {
		var exists bool
		require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='rollout_no_effect_blocks')`).Scan(&exists))
		require.False(t, exists, "failed seed committed an empty authority table")
		var phase string
		require.NoError(t, db.QueryRow(`SELECT json_extract(progress,'$.phase') FROM rollout_commands WHERE id=?`, f.command.ID).Scan(&phase))
		require.Equal(t, "accepted", phase)
	})
}

func TestNoEffectReceiptAndApplyClaimAreSerialized(t *testing.T) {
	f, _, _ := canonicalFixture(t, "apply", "opentofu")
	for attempt := 0; attempt < 6; attempt++ {
		command := f.command
		command.ID, command.Request.ID = fmt.Sprintf("race-%d", attempt), fmt.Sprintf("race-%d", attempt)
		command.EnvironmentID, command.Request.EnvironmentID = fmt.Sprintf("env-%d", attempt), fmt.Sprintf("env-%d", attempt)
		signed := f.signed(t, command)
		_, err := f.engine.importCommand(t.Context(), command, signed.Payload)
		require.NoError(t, err)
		require.NoError(t, f.engine.phase(t.Context(), command.ID, "preparing"))
		start := make(chan struct{})
		claimed, cancelled := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			_, err := f.engine.claim(t.Context(), command.ID, "apply", true)
			claimed <- err
		}()
		go func() {
			<-start
			_, err := f.engine.Cancel(t.Context(), command.ID, "operator")
			cancelled <- err
		}()
		close(start)
		claimErr, cancelErr := <-claimed, <-cancelled
		p, err := f.engine.Get(t.Context(), command.ID)
		require.NoError(t, err)
		if claimErr == nil {
			requireCode(t, cancelErr, "effects_require_reconciliation")
			_, _, ready, err := f.engine.Completion(t.Context(), p)
			require.NoError(t, err)
			require.False(t, ready)
		} else {
			require.NoError(t, cancelErr)
			requireCode(t, claimErr, "approval_required")
			assertNoEffectCompletion(t, f, p, signed.Payload)
		}
	}
}

func TestEffectClaimAndNoEffectFactRollbackTogether(t *testing.T) {
	f, _, _ := canonicalFixture(t, "reviewed", "opentofu")
	signed := f.signed(t, f.command)
	accepted, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
	require.NoError(t, err)
	_, err = f.engine.db.Exec(`CREATE TRIGGER reject_effect_fact BEFORE INSERT ON rollout_no_effect_blocks
 BEGIN SELECT RAISE(ABORT,'owned fixture storage refusal'); END`)
	require.NoError(t, err)
	_, err = f.engine.claim(t.Context(), f.command.ID, "task:external", false)
	require.Error(t, err)
	var claims, facts int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects WHERE rollout=?`, f.command.ID).Scan(&claims))
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_no_effect_blocks WHERE rollout=?`, f.command.ID).Scan(&facts))
	require.Zero(t, claims, "claim survived without its durable fact")
	require.Zero(t, facts)
	after, err := f.engine.Get(t.Context(), f.command.ID)
	require.NoError(t, err)
	require.Equal(t, accepted, after)
	_, err = f.engine.db.Exec(`DROP TRIGGER reject_effect_fact`)
	require.NoError(t, err)
	_, err = f.engine.claim(t.Context(), f.command.ID, "task:external", false)
	require.NoError(t, err)
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects WHERE rollout=?`, f.command.ID).Scan(&claims))
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_no_effect_blocks WHERE rollout=?`, f.command.ID).Scan(&facts))
	require.Equal(t, 1, claims)
	require.Equal(t, 1, facts)
}

func TestCompletedNoopApplyCannotBecomeNoEffectEvidence(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	f.command.ID, f.command.Request.ID = "noop", "noop"
	_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	completed := waitPhase(t, f.engine, f.command.ID, "completed")
	require.True(t, completed.InfrastructureApplied)
	require.Zero(t, completed.ApplyChanges.Added)
	require.Equal(t, 2, operationCount(t, trace, "apply"))
	// Owned retention fixture: a future GC must retain the durable claim fact.
	_, err = f.engine.db.Exec(`DELETE FROM rollout_effects WHERE rollout=?`, completed.ID)
	require.NoError(t, err)
	failed, err := f.engine.update(t.Context(), completed.ID, func(p *Progress) error {
		p.Phase, p.InfrastructureApplied = "failed", false
		return nil
	})
	require.NoError(t, err)
	_, body, ready, err := f.engine.Completion(t.Context(), failed)
	require.NoError(t, err)
	require.True(t, ready)
	var result api.DeploymentResultsUpdateBody
	require.NoError(t, json.Unmarshal(body, &result))
	require.Nil(t, result.NoEffectReceipt)
}
