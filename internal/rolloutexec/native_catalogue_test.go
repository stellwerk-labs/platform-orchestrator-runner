package rolloutexec

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	steps "github.com/stellwerk-labs/platform-orchestrator-runner/internal/pipelinesteps"
	"github.com/stretchr/testify/require"
)

func TestNativeCatalogueRequiresParsedPrivateIdentity(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	for _, invalid := range []*age.X25519Identity{nil, {}} {
		execution := &Executor{}
		require.NoError(t, execution.PrepareNativeCatalogue(identity))
		require.Error(t, execution.PrepareNativeCatalogue(invalid))
		require.Empty(t, execution.catalogueKey, "failed preparation must not retain stale Native authority")
	}
	execution, request := fixture(t)
	request.RunnerUUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	require.Error(t, execution.Validate(request, nil), "Native execution must not fall back to a legacy digest")
	require.NoError(t, execution.PrepareNativeCatalogue(identity))
	require.NoError(t, execution.Validate(request, nil))
}

func TestNativeCatalogueStableAcrossOwnersAndBindsPrivateMaterial(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	execution := publicInventoryFixture(t)
	legacy := execution.CatalogDigest()
	require.NoError(t, execution.PrepareNativeCatalogue(identity))
	original := execution.CatalogDigest()
	require.Len(t, original, 64)
	require.NotEqual(t, legacy, original)
	independent := *execution
	independent.catalogueKey = nil
	parsed, err := age.ParseX25519Identity(identity.String())
	require.NoError(t, err)
	require.NoError(t, independent.PrepareNativeCatalogue(parsed))
	require.Equal(t, original, independent.CatalogDigest())
	independent.Capabilities = []string{"site.alpha", "site.beta"}
	require.Equal(t, original, independent.CatalogDigest())
	independent.Tools = append([]Tool(nil), execution.Tools...)
	independent.Tools[0].Environment = map[string]string{"PRIVATE_CREDENTIAL": "rotated"}
	require.NotEqual(t, original, independent.CatalogDigest())
	independent.Tools = append([]Tool(nil), execution.Tools...)
	independent.Tools[0].Definition.ExecutableSHA256 = strings.Repeat("a", 64)
	require.NotEqual(t, original, independent.CatalogDigest())
	independent.Tools = execution.Tools
	other, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	require.NoError(t, independent.PrepareNativeCatalogue(other))
	require.NotEqual(t, original, independent.CatalogDigest())
	inventory, err := execution.PublicInventory()
	require.NoError(t, err)
	public, err := json.Marshal(inventory)
	require.NoError(t, err)
	for _, secret := range []string{identity.String(), hex.EncodeToString(execution.catalogueKey), "INVENTORY_PRIVATE_CANARY", execution.Tools[0].Definition.Executable} {
		require.NotContains(t, string(public), secret)
	}
}

func TestNativePublicCatalogueRefusesWeakEnvironmentGuessing(t *testing.T) {
	executable, err := filepath.EvalSymlinks("/bin/echo")
	require.NoError(t, err)
	tool := Tool{ID: "known-check", Name: "Known installed check", Phase: "plan_check", Retry: "read_only", Environment: map[string]string{"DB_PASSWORD": "5931"}, Definition: steps.Definition{ProtocolVersion: 1, Name: "check", Version: "1", Phase: "plan_check", Retry: "read_only", Executable: executable, ExecutableSHA256: executableDigest(executable)}}
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	execution := &Executor{Tools: []Tool{tool}, Capabilities: []string{"site.read"}}
	require.NoError(t, execution.PrepareNativeCatalogue(identity))
	public, err := execution.PublicInventory()
	require.NoError(t, err)
	binaries := map[string]string{"opentofu": executableDigest("opentofu"), "terraform": executableDigest("terraform")}
	// The attacker knows installed inputs and the public Recipient, never the private identity.
	publicKey, err := hkdf.Key(sha256.New, []byte(identity.Recipient().String()), nil, "stellwerk.runner.native-catalogue/hmac-sha256/v1", sha256.Size)
	require.NoError(t, err)
	for candidate := 0; candidate < 10000; candidate++ {
		guess := tool
		guess.Environment = map[string]string{"DB_PASSWORD": fmt.Sprintf("%04d", candidate)}
		raw, err := json.Marshal(struct {
			Tools        []Tool            `json:"tools"`
			Capabilities []string          `json:"capabilities"`
			Binaries     map[string]string `json:"binaries"`
			Cost         *costIdentity     `json:"cost,omitempty"`
		}{[]Tool{guess}, []string{"site.read"}, binaries, nil})
		require.NoError(t, err)
		plain := sha256.Sum256(raw)
		if hex.EncodeToString(plain[:]) == public.ToolsDigest {
			t.Fatal("raw SHA dictionary recovered a private environment value")
		}
		mac := hmac.New(sha256.New, publicKey)
		_, _ = mac.Write(raw)
		if hex.EncodeToString(mac.Sum(nil)) == public.ToolsDigest {
			t.Fatal("public Recipient dictionary recovered a private environment value")
		}
	}
}

func TestLegacyCatalogueMatchesImmutableBaselineGolden(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"tofu", "terraform"} {
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte("bounded fake "+name+" binary\n"), 0700))
	}
	t.Setenv("PATH", directory)
	execution := &Executor{Capabilities: []string{"site.beta", "site.alpha"}, Tools: []Tool{{ID: "tool", Name: "Legacy", Phase: "plan_check", Retry: "read_only", Environment: map[string]string{"PRIVATE_FIXTURE": "legacy"}, Definition: steps.Definition{ProtocolVersion: 1, Name: "known", Version: "1", Executable: "/private/fixed-tool", ExecutableSHA256: strings.Repeat("a", 64)}}}}
	require.Equal(t, "264161b4d5bd9465df44a9b15952f583e17a4989a0d244869bedbab032cd1109", execution.CatalogDigest())
}
