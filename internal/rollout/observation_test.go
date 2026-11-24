package rollout

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func observationFixture(t *testing.T, tool string) (*fixture, string) {
	t.Helper()
	f, trace, _ := canonicalFixture(t, "observation", tool)
	f.command.Request.Mode = "observation"
	f.command.Request.EncryptedOutputsRecipient = ""
	f.command.RequiredCapabilities = []string{rolloutexec.ObservationCapability}
	f.config.Executor.Capabilities = []string{rolloutexec.ObservationCapability}
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
	return f, trace
}

func TestObservationCancelledBeforeProcessAdmissionCannotStart(t *testing.T) {
	f, trace := observationFixture(t, "opentofu")
	signed := f.signed(t, f.command)
	command, err := f.engine.verify(signed)
	require.NoError(t, err)
	_, err = f.engine.importCommand(t.Context(), command, signed.Payload)
	require.NoError(t, err)
	cancelled, err := f.engine.Cancel(t.Context(), command.ID, "operator")
	require.NoError(t, err)
	require.Equal(t, "cancelled", cancelled.Phase)
	err = f.engine.observe(t.Context(), command.ID)
	requireCode(t, err, "rollout_stopped")
	_, err = os.Stat(trace)
	require.True(t, os.IsNotExist(err), "cancelled observation launched an IaC subprocess")
}

func TestObservationFailureReturnsOnlySanitizedCompletion(t *testing.T) {
	f, trace := observationFixture(t, "opentofu")
	f.command.Request.Bundle = executionBundle(t, map[string]string{"main.tf": "SECRET_OBSERVATION_DIAGNOSTIC invalid configuration"})
	f.command.Request.BundleSHA256 = digest(f.command.Request.Bundle)
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	failed := waitPhase(t, f.engine, f.command.ID, "failed")
	_, body, ready, err := f.engine.Completion(t.Context(), failed)
	require.NoError(t, err)
	require.True(t, ready)
	var result api.DeploymentResultsUpdateBody
	require.NoError(t, json.Unmarshal(body, &result))
	require.Equal(t, api.Failure, result.Status)
	require.Nil(t, result.ObservationResult)
	require.NotContains(t, string(body), "SECRET_OBSERVATION_DIAGNOSTIC")
	require.Zero(t, operationCount(t, trace, "apply"))
	require.False(t, failed.InfrastructureApplied)
}

func TestObservationSignedAdmissionRejectsMutatingAuthority(t *testing.T) {
	f, _ := observationFixture(t, "opentofu")
	for _, test := range []struct {
		name   string
		change func(*Command)
	}{
		{"strategy", func(c *Command) { c.Strategy = "apply" }},
		{"mode", func(c *Command) { c.Request.Mode = "deploy" }},
		{"capability", func(c *Command) { c.RequiredCapabilities = nil }},
		{"recipe", func(c *Command) { c.Recipe = &RecipeReference{ID: "recipe", Version: 1, Digest: c.ToolsDigest} }},
		{"cost", func(c *Command) { c.Cost = &CostPolicy{} }},
		{"source", func(c *Command) { c.SourcePlan = &SourcePlan{} }},
		{"base", func(c *Command) { c.PlanBase = &PlanBase{} }},
		{"step", func(c *Command) { c.Steps = []rolloutexec.Step{{ID: "task"}} }},
		{"approval", func(c *Command) { c.Approval = Approval{MaxAdd: 1} }},
		{"recipient", func(c *Command) { c.Request.EncryptedOutputsRecipient = "recipient" }},
		{"metadata", func(c *Command) { c.Request.MetadataKey = "metadata" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := f.command
			test.change(&command)
			_, err := f.engine.Submit(context.Background(), f.signed(t, command))
			require.Error(t, err)
		})
	}
	signed := f.signed(t, f.command)
	signed.Payload = append(signed.Payload, ' ')
	_, err := f.engine.Submit(context.Background(), signed)
	requireCode(t, err, "signature_refused")
	var count int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_commands`).Scan(&count))
	require.Zero(t, count, "invalid observation acquired durable admission")
}
