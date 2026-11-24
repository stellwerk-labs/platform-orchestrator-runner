package rolloutexec

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	steps "github.com/stellwerk-labs/platform-orchestrator-runner/internal/pipelinesteps"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stretchr/testify/require"
)

func publicInventoryFixture(t *testing.T) *Executor {
	t.Helper()
	interpreter, err := exec.LookPath("sh")
	require.NoError(t, err)
	interpreter, err = filepath.EvalSymlinks(interpreter)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "private-approved-step")
	require.NoError(t, os.WriteFile(file, []byte("exit 99\n"), 0700))
	return &Executor{Directory: filepath.Join(t.TempDir(), "private-work"), Capabilities: []string{"site.beta", "site.alpha"}, Tools: []Tool{{
		ID: "check", Name: "Check resources", Phase: "plan_check", Retry: "read_only", RequiredCapabilities: []string{"site.beta", "site.alpha"},
		Environment: map[string]string{"PRIVATE_CREDENTIAL": "INVENTORY_PRIVATE_CANARY"},
		Definition: steps.Definition{ProtocolVersion: 1, Name: "check", Version: "1", Phase: "plan_check", Retry: "read_only",
			Executable: file, ExecutableSHA256: fileDigest(t, file), Interpreter: interpreter, InterpreterSHA256: fileDigest(t, interpreter), Capabilities: []string{"site.alpha"}},
	}}}
}

func TestPublicInventoryPreservesPrivateDigestAndProjectsBothRequirements(t *testing.T) {
	e := publicInventoryFixture(t)
	e.Tools[0].RequiredCapabilities = []string{"site.beta"}
	second := e.Tools[0]
	second.ID = "aaa"
	e.Tools = append(e.Tools, second)
	before, err := json.Marshal(e)
	require.NoError(t, err)
	result, err := e.PublicInventory()
	require.NoError(t, err)
	require.Equal(t, e.CatalogDigest(), result.ToolsDigest)
	require.Equal(t, []string{"site.alpha", "site.beta"}, result.Capabilities)
	require.Equal(t, "aaa", result.Tools[0].ID)
	require.Equal(t, []string{"site.alpha", "site.beta"}, result.Tools[1].RequiredCapabilities)
	after, err := json.Marshal(e)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "public sorting must not mutate digest inputs")
	public, err := json.Marshal(result)
	require.NoError(t, err)
	for _, private := range []string{"INVENTORY_PRIVATE_CANARY", "PRIVATE_CREDENTIAL", e.Directory, e.Tools[0].Definition.Executable, e.Tools[0].Definition.Interpreter} {
		require.NotContains(t, string(public), private)
	}
}

func TestPublicInventoryRefusesUnavailableOrMisleadingMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *Executor)
	}{
		{"duplicate tool", func(_ *testing.T, e *Executor) { e.Tools = append(e.Tools, e.Tools[0]) }},
		{"invalid ID", func(_ *testing.T, e *Executor) { e.Tools[0].ID = "invalid/id" }},
		{"control in name", func(_ *testing.T, e *Executor) { e.Tools[0].Name = "Check\nresources" }},
		{"blank name", func(_ *testing.T, e *Executor) { e.Tools[0].Name = "   " }},
		{"long name", func(_ *testing.T, e *Executor) { e.Tools[0].Name = strings.Repeat("界", 121) }},
		{"invalid UTF8", func(_ *testing.T, e *Executor) { e.Tools[0].Name = string([]byte{255}) }},
		{"missing requirement", func(_ *testing.T, e *Executor) { e.Tools[0].Definition.Capabilities = []string{"site.unavailable"} }},
		{"invalid capability", func(_ *testing.T, e *Executor) { e.Capabilities = append(e.Capabilities, "INVALID") }},
		{"too many capabilities", func(_ *testing.T, e *Executor) {
			for i := range 33 {
				e.Capabilities = append(e.Capabilities, fmt.Sprintf("cap.%d", i))
			}
		}},
		{"too many tools", func(_ *testing.T, e *Executor) {
			for range 32 {
				e.Tools = append(e.Tools, e.Tools[0])
			}
		}},
		{"unsafe check", func(_ *testing.T, e *Executor) { e.Tools[0].Retry, e.Tools[0].Definition.Retry = "unsafe", "unsafe" }},
		{"changed executable", func(t *testing.T, e *Executor) {
			require.NoError(t, os.WriteFile(e.Tools[0].Definition.Executable, []byte("changed"), 0700))
		}},
		{"missing executable", func(_ *testing.T, e *Executor) { e.Tools[0].Definition.Executable += ".missing" }},
		{"changed interpreter", func(_ *testing.T, e *Executor) { e.Tools[0].Definition.InterpreterSHA256 = strings.Repeat("0", 64) }},
		{"no IaC binaries", func(t *testing.T, _ *Executor) { t.Setenv("PATH", t.TempDir()) }},
		{"cost asserted without configuration", func(_ *testing.T, e *Executor) { e.Capabilities = append(e.Capabilities, CostCapability) }},
		{"pricing asserted without configuration", func(_ *testing.T, e *Executor) { e.Capabilities = append(e.Capabilities, LocalPricingCapability) }},
		{"contradictory pricing mode", func(_ *testing.T, e *Executor) {
			e.Cost = &runner.CostConfiguration{APIKey: "INVENTORY_PRIVATE_CANARY", Binary: e.Tools[0].Definition.Interpreter, PricingAccess: "network"}
			e.Capabilities = append(e.Capabilities, CostCapability, LocalPricingCapability)
		}},
		{"unavailable cost binary", func(_ *testing.T, e *Executor) {
			e.Cost = &runner.CostConfiguration{APIKey: "INVENTORY_PRIVATE_CANARY", Binary: "/unavailable/infracost"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := publicInventoryFixture(t)
			test.change(t, e)
			_, err := e.PublicInventory()
			require.Error(t, err)
			require.NotContains(t, err.Error(), "INVENTORY_PRIVATE_CANARY")
			require.NotContains(t, err.Error(), e.Directory)
		})
	}
}

func TestPublicInventoryEmptyArraysAndUnicodeNameBoundary(t *testing.T) {
	e := publicInventoryFixture(t)
	e.Tools[0].Name = strings.Repeat("界", 120)
	_, err := e.PublicInventory()
	require.NoError(t, err)
	e.Tools, e.Capabilities = nil, nil
	result, err := e.PublicInventory()
	require.NoError(t, err)
	require.NotNil(t, result.Tools)
	require.NotNil(t, result.Capabilities)
}
