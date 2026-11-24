package rollout

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCostActivationRevisionPreservesLegacyEncodingAndBindsNewAuthority(t *testing.T) {
	legacy, err := json.Marshal(CostPolicy{Enabled: true})
	require.NoError(t, err)
	require.JSONEq(t, `{"enabled":true}`, string(legacy))
	require.NotContains(t, string(legacy), "policy_revision")
	current, err := json.Marshal(CostPolicy{Enabled: true, PolicyRevision: 7})
	require.NoError(t, err)
	require.Contains(t, string(current), `"policy_revision":7`)
	require.NotEqual(t, digest(legacy), digest(current), "the command subject binds the installation activation revision")
	engine := &Engine{}
	requireCode(t, engine.validateCost(Command{Cost: &CostPolicy{Enabled: true, PolicyRevision: -1}}), "invalid_cost_policy")
}
