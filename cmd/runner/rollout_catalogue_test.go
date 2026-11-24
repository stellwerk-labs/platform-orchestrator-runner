package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	steps "github.com/stellwerk-labs/platform-orchestrator-runner/internal/pipelinesteps"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func catalogueCLIFixture(t *testing.T) rolloutConfig {
	t.Helper()
	root := t.TempDir()
	site := filepath.Join(root, "retained-site")
	require.NoError(t, os.Mkdir(site, 0700))
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(site, "identity.agekey"), []byte(identity.String()), 0400))
	pgIdentity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	pgKey := filepath.Join(root, "native-pg.agekey")
	require.NoError(t, os.WriteFile(pgKey, []byte(pgIdentity.String()), 0400))
	require.NoError(t, os.WriteFile(filepath.Join(site, "execution.lock"), nil, 0600))
	db, err := sql.Open("sqlite", filepath.Join(site, "rollouts.sqlite"))
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE retained_fixture (marker TEXT); INSERT INTO retained_fixture VALUES ('PRIVATE_INVENTORY_CLI_CANARY')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	shell, err := exec.LookPath("sh")
	require.NoError(t, err)
	shell, err = filepath.EvalSymlinks(shell)
	require.NoError(t, err)
	shellBytes, err := os.ReadFile(shell)
	require.NoError(t, err)
	file := filepath.Join(root, "private-approved-step")
	content := []byte("exit 99 # PRIVATE_INVENTORY_CLI_CANARY\n")
	require.NoError(t, os.WriteFile(file, content, 0700))
	return rolloutConfig{Directory: site, OrgID: "private-org", RunnerID: "private-runner", Listen: "not-a-listener", GatewayURL: "http://127.0.0.1:1",
		PublicKeyFile: filepath.Join(root, "absent-central-key"), AgentPrivateKeyFile: filepath.Join(root, "absent-agent-key"), APITokenFile: filepath.Join(root, "absent-api-token"),
		Postgres:     &rollout.PostgresConfig{DSNFile: filepath.Join(root, "absent-postgres-dsn"), IdentityFile: pgKey},
		Capabilities: []string{"site.read"}, Tools: []rolloutexec.Tool{{ID: "check", Name: "Validate resources", Phase: "plan_check", Retry: "read_only",
			Environment: map[string]string{"PRIVATE_CREDENTIAL": "PRIVATE_INVENTORY_CLI_CANARY"},
			Definition: steps.Definition{ProtocolVersion: 1, Name: "check", Version: "1", Phase: "plan_check", Retry: "read_only", Executable: file,
				ExecutableSHA256: fmt.Sprintf("%x", sha256.Sum256(content)), Interpreter: shell, InterpreterSHA256: fmt.Sprintf("%x", sha256.Sum256(shellBytes)), Capabilities: []string{"site.read"}},
		}}}
}

func catalogueStorageSnapshot(t *testing.T, directory string) map[string]string {
	t.Helper()
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files[path] = info.Mode().String()
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] += fmt.Sprintf(":%x", sha256.Sum256(data))
		}
		return nil
	}))
	return files
}

func prepareCatalogueExecutor(t *testing.T, cfg rolloutConfig, execution *rolloutexec.Executor) {
	t.Helper()
	if cfg.Postgres == nil {
		return
	}
	identity, err := rollout.LoadStorageIdentity(cfg.Postgres.IdentityFile)
	require.NoError(t, err)
	require.NoError(t, execution.PrepareNativeCatalogue(identity))
}

func catalogueCLI(t *testing.T, cfg rolloutConfig) func([]byte, bool) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	binary := filepath.Join(t.TempDir(), "runner")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "%s", output)
	configFile := filepath.Join(t.TempDir(), "config.json")
	privateValues := []string{"PRIVATE_INVENTORY_CLI_CANARY", "PRIVATE_CREDENTIAL", cfg.Directory, cfg.Tools[0].Definition.Executable, cfg.Tools[0].Definition.Interpreter, cfg.OrgID, cfg.RunnerID}
	if cfg.Postgres != nil {
		privateValues = append(privateValues, cfg.Postgres.IdentityFile)
		key, err := os.ReadFile(cfg.Postgres.IdentityFile)
		require.NoError(t, err)
		privateValues = append(privateValues, string(key))
	}
	return func(raw []byte, success bool) []byte {
		t.Helper()
		require.NoError(t, os.WriteFile(configFile, raw, 0600))
		invocation, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		command := exec.CommandContext(invocation, binary, "rollout-catalogue")
		command.Env = append(os.Environ(), "ROLLOUT_CONFIG_FILE="+configFile)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		require.NoError(t, invocation.Err(), "catalogue export must finish without blocking")
		if success {
			require.NoError(t, err, "%s", stderr.Bytes())
		} else {
			require.Error(t, err)
			require.Empty(t, stdout.String())
		}
		for _, private := range privateValues {
			require.NotContains(t, stdout.String()+stderr.String(), private)
		}
		return stdout.Bytes()
	}
}

func TestRolloutCatalogueCLIIsOfflineAndPrivate(t *testing.T) {
	t.Setenv("ROLLOUT_WORKER_ID", "invalid/unused/offline-export")
	cfg := catalogueCLIFixture(t)
	before := catalogueStorageSnapshot(t, cfg.Directory)
	run := catalogueCLI(t, cfg)
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	var result rolloutCatalogueExport
	decoder := json.NewDecoder(bytes.NewReader(run(raw, true)))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&result))
	require.ErrorIs(t, decoder.Decode(new(any)), io.EOF)
	require.Equal(t, rollout.NativeRuntimeVersion, result.NativeExecution.RuntimeVersion)
	require.Contains(t, result.SupportedProtocols, rollout.NativePipelineVersion)
	require.Contains(t, result.SupportedProtocols, "stellwerk.pipeline.plan-apply/v2")
	require.NotContains(t, result.SupportedProtocols, rollout.PipelineVersion)
	nativePG := cfg.Postgres
	// Inventory must precede enrollment, but missing authority must still stop every runtime entry point.
	public, _, keyErr := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, keyErr)
	runtimeConfig := rollout.Config{Postgres: cfg.Postgres, Directory: filepath.Join(t.TempDir(), "uninitialized"), OrgID: cfg.OrgID, RunnerID: cfg.RunnerID, RunnerUUID: cfg.RunnerUUID, PublicKey: public, Executor: &rolloutexec.Executor{}}
	engine, openErr := rollout.Open(t.Context(), runtimeConfig)
	require.Nil(t, engine)
	require.EqualError(t, openErr, "runner_uuid_refused")
	require.EqualError(t, rollout.InitializePostgres(t.Context(), runtimeConfig), "runner_uuid_refused")
	_, statErr := os.Stat(runtimeConfig.Directory)
	require.True(t, os.IsNotExist(statErr))
	cfg.Postgres = nil
	cfg.RunnerUUID = ""
	legacy, encodeErr := json.Marshal(cfg)
	require.NoError(t, encodeErr)
	var legacyResult rolloutCatalogueExport
	require.NoError(t, json.Unmarshal(run(legacy, true), &legacyResult))
	require.Equal(t, rollout.RuntimeVersion, legacyResult.NativeExecution.RuntimeVersion)
	require.Equal(t, []string{rollout.PipelineVersion, "stellwerk.pipeline.plan-apply/v1", "environment-observation/v1"}, legacyResult.SupportedProtocols)
	cfg.Postgres = nativePG
	expected := &rolloutexec.Executor{Tools: cfg.Tools, Capabilities: cfg.Capabilities}
	prepareCatalogueExecutor(t, cfg, expected)
	require.Equal(t, expected.CatalogDigest(), result.NativeExecution.ToolsDigest)
	require.Equal(t, []string{"site.read"}, result.NativeExecution.Tools[0].RequiredCapabilities)
	for _, invalid := range [][]byte{append(append([]byte{}, raw...), []byte(" {}")...), []byte(`{"unknown":"PRIVATE_INVENTORY_CLI_CANARY"}`), []byte("{"), append(append([]byte{}, raw...), bytes.Repeat([]byte(" "), 1<<20)...)} {
		run(invalid, false)
	}
	cfg.Capabilities = append(cfg.Capabilities, rolloutexec.CostCapability)
	raw, err = json.Marshal(cfg)
	require.NoError(t, err)
	run(raw, false)
	require.Equal(t, before, catalogueStorageSnapshot(t, cfg.Directory), "export must not touch the retained journal, key or lock")
}

func TestRolloutCatalogueCLINativeRequiresProvisionedIdentity(t *testing.T) {
	cfg := catalogueCLIFixture(t)
	run := catalogueCLI(t, cfg)
	before := catalogueStorageSnapshot(t, cfg.Directory)
	key := cfg.Postgres.IdentityFile
	valid, err := os.ReadFile(key)
	require.NoError(t, err)
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	run(raw, true)
	for _, invalid := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"empty", nil, 0400},
		{"malformed", []byte("not-an-age-identity"), 0400},
		{"comment", []byte("# unsupported comment\n"), 0400},
		{"world-readable", valid, 0644},
		{"oversized", append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), 1025)...), 0400},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			require.NoError(t, os.Chmod(key, 0600))
			require.NoError(t, os.WriteFile(key, invalid.data, 0600))
			require.NoError(t, os.Chmod(key, invalid.mode))
			run(raw, false)
			require.Equal(t, before, catalogueStorageSnapshot(t, cfg.Directory))
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can read mode000 files")
		}
		require.NoError(t, os.Chmod(key, 0600))
		require.NoError(t, os.WriteFile(key, valid, 0600))
		require.NoError(t, os.Chmod(key, 0000))
		_, err := os.ReadFile(key)
		require.ErrorIs(t, err, os.ErrPermission)
		run(raw, false)
		require.Equal(t, before, catalogueStorageSnapshot(t, cfg.Directory))
	})
	require.NoError(t, os.Remove(key))
	t.Run("directory", func(t *testing.T) {
		require.NoError(t, os.Mkdir(key, 0700))
		run(raw, false)
		require.Equal(t, before, catalogueStorageSnapshot(t, cfg.Directory))
		require.NoError(t, os.Remove(key))
	})
	t.Run("fifo", func(t *testing.T) {
		require.NoError(t, syscall.Mkfifo(key, 0600))
		run(raw, false)
		require.Equal(t, before, catalogueStorageSnapshot(t, cfg.Directory))
		require.NoError(t, os.Remove(key))
	})
	run(raw, false)
	require.Equal(t, before, catalogueStorageSnapshot(t, cfg.Directory))
	_, err = os.Stat(key)
	require.True(t, os.IsNotExist(err), "export must not generate an identity")
}

func TestRolloutCatalogueCLICostUsesOnlyLocalConfiguration(t *testing.T) {
	cfg := catalogueCLIFixture(t)
	before := catalogueStorageSnapshot(t, cfg.Directory)
	run := catalogueCLI(t, cfg)
	key := filepath.Join(t.TempDir(), "private-pricing-key")
	require.NoError(t, os.WriteFile(key, []byte("PRIVATE_INVENTORY_CLI_CANARY"), 0400))
	for _, pricing := range []string{"network", "local"} {
		cfg.Cost = &rolloutCostConfiguration{APIKeyFile: key, Binary: cfg.Tools[0].Definition.Interpreter, PricingAccess: pricing, Endpoint: "http://127.0.0.1:1"}
		cost, err := loadRolloutCost(cfg.Cost)
		require.NoError(t, err)
		capability := rolloutexec.NetworkPricingCapability
		if pricing == "local" {
			capability = rolloutexec.LocalPricingCapability
		}
		expected := &rolloutexec.Executor{Tools: cfg.Tools, Cost: cost, Capabilities: append(append([]string{}, cfg.Capabilities...), rolloutexec.CostCapability, capability)}
		prepareCatalogueExecutor(t, cfg, expected)
		raw, err := json.Marshal(cfg)
		require.NoError(t, err)
		var result rolloutCatalogueExport
		require.NoError(t, json.Unmarshal(run(raw, true), &result))
		require.Contains(t, result.Capabilities, capability)
		require.Contains(t, result.Capabilities, rolloutexec.CostCapability)
		require.Equal(t, expected.CatalogDigest(), result.NativeExecution.ToolsDigest)
	}
	cfg.Cost.Binary += ".missing"
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	run(raw, false)
	cfg.Cost.Binary = cfg.Tools[0].Definition.Interpreter
	require.NoError(t, os.Chmod(key, 0644))
	raw, err = json.Marshal(cfg)
	require.NoError(t, err)
	run(raw, false)
	require.Equal(t, before, catalogueStorageSnapshot(t, cfg.Directory))
}

func TestRolloutCatalogueCLIBoundsCompletePublicManifest(t *testing.T) {
	cfg := catalogueCLIFixture(t)
	for i := range 32 {
		cfg.Capabilities = append(cfg.Capabilities, fmt.Sprintf("cap.%02d.%s", i, strings.Repeat("a", 120)))
	}
	cfg.Capabilities = cfg.Capabilities[1:]
	tool := cfg.Tools[0]
	tool.Definition.Capabilities = cfg.Capabilities
	cfg.Tools = nil
	for i := range 32 {
		tool.ID = fmt.Sprintf("tool-%02d", i)
		cfg.Tools = append(cfg.Tools, tool)
	}
	expected := &rolloutexec.Executor{Tools: cfg.Tools, Capabilities: cfg.Capabilities}
	prepareCatalogueExecutor(t, cfg, expected)
	inventory, err := expected.PublicInventory()
	require.NoError(t, err)
	encoded, err := json.Marshal(rolloutCatalogueManifest{ContractVersion: "stellwerk.runner.catalogue/v1", RuntimeVersion: rollout.RuntimeVersion, ToolsDigest: inventory.ToolsDigest, Tools: inventory.Tools})
	require.NoError(t, err)
	require.Greater(t, len(encoded), 64<<10)
	file := filepath.Join(t.TempDir(), "oversized-inventory.json")
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, raw, 0600))
	t.Setenv("ROLLOUT_CONFIG_FILE", file)
	err = exportRolloutCatalogue()
	require.ErrorContains(t, err, "size limit")
}

func TestRolloutCatalogueCLINativeBackendValidatesWithoutStorage(t *testing.T) {
	cfg := catalogueCLIFixture(t)
	before := catalogueStorageSnapshot(t, cfg.Directory)
	run := catalogueCLI(t, cfg)
	ca, err := os.ReadFile("../../internal/rolloutexec/testdata/native-backend-ca.pem")
	require.NoError(t, err)
	backend := clientcmdapi.Config{CurrentContext: "selected",
		Contexts:  map[string]*clientcmdapi.Context{"selected": {Cluster: "cluster", AuthInfo: "user"}},
		Clusters:  map[string]*clientcmdapi.Cluster{"cluster": {Server: "https://private-cluster.test", CertificateAuthorityData: ca}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"user": {Token: "PRIVATE_INVENTORY_CLI_CANARY"}}}
	config, err := clientcmd.Write(backend)
	require.NoError(t, err)
	cfg.KubernetesBackendConfigPath = filepath.Join(t.TempDir(), "PRIVATE_INVENTORY_CLI_CANARY")
	require.NoError(t, os.WriteFile(cfg.KubernetesBackendConfigPath, config, 0600))
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	var exported rolloutCatalogueExport
	require.NoError(t, json.Unmarshal(run(raw, true), &exported))
	expected := &rolloutexec.Executor{Tools: cfg.Tools, Capabilities: cfg.Capabilities}
	prepareCatalogueExecutor(t, cfg, expected)
	require.Equal(t, expected.CatalogDigest(), exported.NativeExecution.ToolsDigest)
	for _, variable := range []string{"KUBE_HOST", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv(variable, "PRIVATE_INVENTORY_CLI_CANARY")
			run(raw, false)
		})
	}
	backend.Clusters["cluster"].ProxyURL = "http://127.0.0.1:1"
	config, err = clientcmd.Write(backend)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfg.KubernetesBackendConfigPath, config, 0600))
	t.Setenv("HTTPS_PROXY", "PRIVATE_INVENTORY_CLI_CANARY")
	run(raw, true) // Explicit proxy validation is offline, not a reachability claim.
	require.NoError(t, os.WriteFile(cfg.KubernetesBackendConfigPath, []byte("{"), 0600))
	run(raw, false)
	cfg.Postgres = nil
	cfg.KubernetesBackendConfigPath = "/not-read-by-legacy-export"
	legacy, err := json.Marshal(cfg)
	require.NoError(t, err)
	run(legacy, true)
	require.Equal(t, before, catalogueStorageSnapshot(t, cfg.Directory))
}
