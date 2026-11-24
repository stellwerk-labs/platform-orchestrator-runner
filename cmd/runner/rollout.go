package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutagent"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

type rolloutConfig struct {
	ActivationFile              string                    `json:"activation_file,omitempty"`
	Postgres                    *rollout.PostgresConfig   `json:"postgres,omitempty"`
	Cost                        *rolloutCostConfiguration `json:"cost,omitempty"`
	KubernetesBackendConfigPath string                    `json:"kubernetes_backend_config_path"`
	Directory                   string                    `json:"directory"`
	OrgID                       string                    `json:"org_id"`
	RunnerID                    string                    `json:"runner_id"`
	RunnerUUID                  string                    `json:"runner_uuid,omitempty"`
	PublicKeyFile               string                    `json:"public_key_file"`
	AgentPrivateKeyFile         string                    `json:"agent_private_key_file"`
	GatewayURL                  string                    `json:"gateway_url"`
	GatewayCAFile               string                    `json:"gateway_ca_file"`
	Listen                      string                    `json:"listen"`
	APITokenFile                string                    `json:"api_token_file"`
	Tools                       []rolloutexec.Tool        `json:"tools"`
	Capabilities                []string                  `json:"capabilities"`
}

func runRollout(ctx context.Context) (resultErr error) {
	cfg, public, err := loadRolloutConfig()
	if err != nil {
		return err
	}
	agentKey, err := os.ReadFile(cfg.AgentPrivateKeyFile)
	if err != nil {
		return errors.New("read runner gateway identity")
	}
	token, err := os.ReadFile(cfg.APITokenFile)
	if err != nil || len(token) < 32 {
		return errors.New("read local operator credential")
	}
	cost, err := loadRolloutCost(cfg.Cost)
	if err != nil {
		return err
	}
	if cost != nil {
		pricing := rolloutexec.NetworkPricingCapability
		if cost.PricingAccess == "local" {
			pricing = rolloutexec.LocalPricingCapability
		}
		cfg.Capabilities = append(cfg.Capabilities, rolloutexec.CostCapability, pricing)
	}
	execution := &rolloutexec.Executor{Cost: cost, KubernetesBackendConfigPath: cfg.KubernetesBackendConfigPath, Tools: cfg.Tools, Capabilities: cfg.Capabilities}
	if err := validateRolloutActivation(cfg, execution); err != nil {
		return err
	}
	engine, err := rollout.Open(ctx, rollout.Config{Postgres: cfg.Postgres, Directory: cfg.Directory, OrgID: cfg.OrgID, RunnerID: cfg.RunnerID, RunnerUUID: cfg.RunnerUUID, PublicKey: public, Executor: execution})
	if err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, engine.Close(shutdown))
	}()
	client, err := gatewayapi.NewClient(gatewayapi.ClientConfig{BaseURL: cfg.GatewayURL, OrganizationID: cfg.OrgID, RunnerID: cfg.RunnerID, RunnerUUID: cfg.RunnerUUID, PrivateKey: agentKey, CAFile: cfg.GatewayCAFile})
	if err != nil {
		return err
	}
	localCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	transportID := cfg.RunnerID
	if cfg.RunnerUUID != "" {
		transportID, err = hmessaging.NativeRunnerToken(cfg.RunnerUUID)
		if err != nil {
			cancel()
			return err
		}
	}
	done := make(chan struct{})
	go func() { defer close(done); rolloutagent.Run(localCtx, engine, client, cfg.OrgID, transportID) }()
	defer func() {
		cancel()
		<-done
	}()
	server := &http.Server{Addr: cfg.Listen, Handler: rolloutagent.Handler(engine, strings.TrimSpace(string(token)), map[string]any{"site": map[string]any{"org_id": cfg.OrgID, "runner_id": cfg.RunnerID, "capabilities": cfg.Capabilities, "tools_digest": engine.ToolsDigest()}}), ReadHeaderTimeout: 5 * time.Second}
	return serveRollout(ctx, engine, server)
}

func serveRollout(ctx context.Context, engine *rollout.Engine, server *http.Server) (resultErr error) {
	serverResult := make(chan error, 1)
	go func() {
		defer close(serverResult)
		serverResult <- server.ListenAndServe()
	}()
	defer func() {
		// Public Shutdown preserves PENDING; retain its first failure before the
		// caller's idempotent fallback Close and Agent cancellation.
		shutdown, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		resultErr = errors.Join(resultErr, engine.Close(shutdown), server.Shutdown(shutdown))
		for err := range serverResult {
			if !errors.Is(err, http.ErrServerClosed) {
				resultErr = errors.Join(resultErr, err)
			}
		}
		select {
		case err := <-engine.Fatal():
			resultErr = errors.Join(resultErr, err)
		default:
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			resultErr = errors.Join(resultErr, ctx.Err())
		}
	}()
	stopRequest := ctx.Done()
	draining := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for !draining || engine.WorkerStatus().ActiveStages != 0 {
		select {
		case err := <-engine.Fatal():
			return err
		case err := <-serverResult:
			return err
		case <-stopRequest:
			engine.Drain()
			draining, stopRequest = true, nil
		case <-ticker.C:
		}
	}
	select {
	case err := <-engine.Fatal():
		return err
	case err := <-serverResult:
		return err
	default:
	}
	return nil
}

func loadRolloutConfig() (rolloutConfig, ed25519.PublicKey, error) {
	var cfg rolloutConfig
	file, err := os.Open(os.Getenv("ROLLOUT_CONFIG_FILE"))
	if err != nil {
		return cfg, nil, errors.New("open local rollout configuration")
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(http.MaxBytesReader(nil, file, 1<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil {
		return cfg, nil, errors.New("invalid local rollout configuration")
	}
	if err := bindRolloutWorkerID(&cfg, os.Getenv("ROLLOUT_WORKER_ID")); err != nil {
		return cfg, nil, err
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return cfg, nil, errors.New("experimental rollout operator API requires loopback")
	}
	raw, err := os.ReadFile(cfg.PublicKeyFile)
	if err != nil {
		return cfg, nil, errors.New("read rollout verification key")
	}
	public, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(public) != ed25519.PublicKeySize {
		return cfg, nil, errors.New("invalid rollout verification key")
	}
	return cfg, public, nil
}

var rolloutWorkerID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func bindRolloutWorkerID(cfg *rolloutConfig, workerID string) error {
	if workerID == "" {
		return nil
	}
	if cfg.Postgres == nil || !rolloutWorkerID.MatchString(workerID) {
		return errors.New("invalid rollout worker identity binding")
	}
	if cfg.Postgres.WorkerID != "" && cfg.Postgres.WorkerID != workerID {
		return errors.New("conflicting rollout worker identity binding")
	}
	cfg.Postgres.WorkerID = workerID
	return nil
}

// Initialization does not authenticate a gateway or launch workflows.
func initializeRolloutStorage(ctx context.Context) error {
	cfg, public, err := loadRolloutConfig()
	if err != nil {
		return err
	}
	cost, err := loadRolloutCost(cfg.Cost)
	if err != nil {
		return err
	}
	if cost != nil {
		pricing := rolloutexec.NetworkPricingCapability
		if cost.PricingAccess == "local" {
			pricing = rolloutexec.LocalPricingCapability
		}
		cfg.Capabilities = append(cfg.Capabilities, rolloutexec.CostCapability, pricing)
	}
	execution := &rolloutexec.Executor{Cost: cost, KubernetesBackendConfigPath: cfg.KubernetesBackendConfigPath, Tools: cfg.Tools, Capabilities: cfg.Capabilities}
	return rollout.InitializePostgres(ctx, rollout.Config{Postgres: cfg.Postgres, Directory: cfg.Directory, OrgID: cfg.OrgID, RunnerID: cfg.RunnerID, RunnerUUID: cfg.RunnerUUID, PublicKey: public, Executor: execution})
}
