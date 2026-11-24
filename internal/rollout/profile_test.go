package rollout

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stretchr/testify/require"
)

func TestProfileAuthorityRefusesBeforeStorage(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	valid := uuid.NewString()
	for _, test := range []struct {
		name, value string
		postgres    bool
	}{
		{"missing", "", true}, {"nil", uuid.Nil.String(), true}, {"uppercase", strings.ToUpper(valid), true}, {"compact", strings.ReplaceAll(valid, "-", ""), true}, {"malformed", "not-a-uuid", true}, {"sqlite_uuid", valid, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{OrgID: "org", RunnerID: "site", RunnerUUID: test.value, PublicKey: public, Executor: &rolloutexec.Executor{}, Directory: filepath.Join(t.TempDir(), "absent")}
			if test.postgres {
				cfg.Postgres = &PostgresConfig{}
			}
			engine, openErr := Open(t.Context(), cfg)
			require.Nil(t, engine)
			requireCode(t, openErr, "runner_uuid_refused")
			requireCode(t, InitializePostgres(t.Context(), cfg), "runner_uuid_refused")
			_, statErr := os.Stat(cfg.Directory)
			require.True(t, os.IsNotExist(statErr), "invalid authority mutated storage")
		})
	}
	runtime, err := RuntimeForProfile(true, valid)
	require.NoError(t, err)
	require.Equal(t, NativeRuntimeVersion, runtime)
	runtime, err = RuntimeForProfile(false, "")
	require.NoError(t, err)
	require.Equal(t, RuntimeVersion, runtime)
}

func TestSQLiteRetainsOnlyLegacyCommandAuthority(t *testing.T) {
	f := newFixture(t)
	for _, test := range []struct {
		name   string
		change func(*Command)
	}{
		{"native_version", func(c *Command) { c.Version = NativePipelineVersion }},
		{"native_uuid", func(c *Command) { c.RunnerUUID = uuid.NewString(); c.Request.RunnerUUID = c.RunnerUUID }},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := f.command
			test.change(&command)
			_, err := f.engine.Submit(t.Context(), f.signed(t, command))
			requireCode(t, err, "command_scope_refused")
		})
	}
	var count int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_commands`).Scan(&count))
	require.Zero(t, count)
}

func TestNativeCostQualificationUsesBoundPipelineVersion(t *testing.T) {
	execution := &rolloutexec.Executor{Cost: &runner.CostConfiguration{PricingAccess: "local"}}
	engine := &Engine{config: Config{Postgres: &PostgresConfig{}}, executor: execution}
	command := Command{Version: NativePipelineVersion, Strategy: "reviewed", Cost: &CostPolicy{Enabled: true, PolicyRevision: 7}, RequiredCapabilities: []string{rolloutexec.CostCapability, rolloutexec.LocalPricingCapability}}
	require.NoError(t, engine.validateCost(command))
	command.Version = PipelineVersion
	requireCode(t, engine.validateCost(command), "cost_capability_refused")
	command.Version = NativePipelineVersion
	command.Cost.PolicyRevision = -1
	requireCode(t, engine.validateCost(command), "invalid_cost_policy")
}
