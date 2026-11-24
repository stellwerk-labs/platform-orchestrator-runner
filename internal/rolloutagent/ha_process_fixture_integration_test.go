//go:build integration && (linux || darwin)

package rolloutagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stretchr/testify/require"
)

type haProcessFixture struct {
	wire        *transportFixture
	db          *sql.DB
	pg          rollout.PostgresConfig
	root        string
	binary      string
	token       string
	config      map[string]any
	workers     []*haWorkerProcess
	artifacts   string
	initialized bool
	runnerUUID  string
}

type haWorkerProcess struct {
	command *exec.Cmd
	done    chan error
	url     string
	log     string
	exited  bool
}

func newHAProcessFixture(t *testing.T) *haProcessFixture {
	t.Helper()
	binary := os.Getenv("HA_RUNNER_BINARY")
	require.True(t, filepath.IsAbs(binary), "HA_RUNNER_BINARY must select an explicitly built Runner")
	info, err := os.Stat(binary)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular() && info.Mode().Perm()&0100 != 0)
	dsnFile := os.Getenv("RUNNER_POSTGRES_TEST_DSN_FILE")
	require.NotEmpty(t, dsnFile, "the required HA gate needs its disposable PostgreSQL DSN file")
	dsn, err := os.ReadFile(dsnFile)
	require.NoError(t, err)
	db, err := sql.Open("postgres", string(dsn))
	require.NoError(t, err)
	require.NoError(t, db.PingContext(t.Context()))
	runnerUUID := uuid.NewString()
	wire := newTransportFixtureForUUID(t, "reviewed", runnerUUID)
	wire.closeEngine(t)
	root := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	key := filepath.Join(root, "shared.agekey")
	require.NoError(t, os.WriteFile(key, []byte(identity.String()), 0400))
	require.NoError(t, wire.config.Executor.PrepareNativeCatalogue(identity))
	schema := "ha_process_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	f := &haProcessFixture{wire: wire, db: db, root: root, binary: binary, token: uuid.NewString() + uuid.NewString(), runnerUUID: runnerUUID,
		pg: rollout.PostgresConfig{Concurrency: 4, WorkerConcurrency: 2, DSNFile: dsnFile, IdentityFile: key, WorkerID: "worker-0", ApplicationSchema: schema, SystemSchema: schema + "_dbos"}}
	t.Cleanup(func() {
		defer func() { require.NoError(t, db.Close()) }()
		defer func() {
			_, dropErr := db.Exec(`DROP SCHEMA IF EXISTS ` + pq.QuoteIdentifier(schema) + ` CASCADE; DROP SCHEMA IF EXISTS ` + pq.QuoteIdentifier(schema+"_dbos") + ` CASCADE`)
			require.NoError(t, dropErr)
		}()
		if t.Failed() {
			f.retainFailure(t)
		}
	})
	f.writeCommonConfig(t)
	return f
}

func (f *haProcessFixture) initialize(t *testing.T) {
	t.Helper()
	initConfig, _ := f.workerConfig(t, "initialize", f.pg.DSNFile)
	initialize := exec.CommandContext(t.Context(), f.binary, "rollout-storage-init")
	initialize.Env = append(os.Environ(), "ROLLOUT_CONFIG_FILE="+initConfig)
	output, err := initialize.CombinedOutput()
	require.NoError(t, err, "isolated storage initialization: %s", output)
	f.initialized = true
}

func (f *haProcessFixture) writeCommonConfig(t *testing.T) {
	t.Helper()
	public, agent, token := filepath.Join(f.root, "public.key"), filepath.Join(f.root, "agent.pem"), filepath.Join(f.root, "operator.token")
	require.NoError(t, os.WriteFile(public, []byte(base64.StdEncoding.EncodeToString(f.wire.config.PublicKey)), 0600))
	require.NoError(t, os.WriteFile(agent, f.wire.clientConfig.PrivateKey, 0600))
	require.NoError(t, os.WriteFile(token, []byte(f.token), 0600))
	f.config = map[string]any{"org_id": f.wire.config.OrgID, "runner_id": f.wire.config.RunnerID, "runner_uuid": f.runnerUUID,
		"public_key_file": public, "agent_private_key_file": agent, "api_token_file": token,
		"gateway_url": f.wire.clientConfig.BaseURL, "gateway_ca_file": f.wire.clientConfig.CAFile,
		"tools": f.wire.config.Executor.Tools, "capabilities": f.wire.config.Executor.Capabilities}
}

func (f *haProcessFixture) workerConfig(t *testing.T, slot, dsnFile string) (string, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	directory, err := os.MkdirTemp(f.root, slot+"-")
	require.NoError(t, err)
	pg := f.pg
	pg.WorkerID = slot
	pg.DSNFile = dsnFile
	config := make(map[string]any, len(f.config)+3)
	for key, value := range f.config {
		config[key] = value
	}
	config["directory"], config["listen"], config["postgres"] = directory, address, pg
	if slot != "initialize" && f.binary != os.Getenv("NATIVE_FORMAT6_RUNNER_BINARY") && config["activation_file"] == nil {
		config["activation_file"] = f.activation(t, pg)
	}
	body, err := json.Marshal(config)
	require.NoError(t, err)
	path := filepath.Join(directory, "config.json")
	require.NoError(t, os.WriteFile(path, body, 0600))
	return path, "http://" + address
}

func (f *haProcessFixture) start(t *testing.T, slot string, ready bool) *haWorkerProcess {
	t.Helper()
	return f.startWithDSN(t, slot, f.pg.DSNFile, ready)
}

func (f *haProcessFixture) startWithDSN(t *testing.T, slot, dsnFile string, ready bool) *haWorkerProcess {
	t.Helper()
	if !f.initialized {
		f.initialize(t)
	}
	config, url := f.workerConfig(t, slot, dsnFile)
	worker := &haWorkerProcess{command: exec.Command(f.binary, "rollout"), done: make(chan error, 1), url: url, log: filepath.Join(filepath.Dir(config), "process.log")}
	worker.command.Env = append(os.Environ(), "ROLLOUT_CONFIG_FILE="+config)
	log, err := os.OpenFile(worker.log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	require.NoError(t, err)
	worker.command.Stdout, worker.command.Stderr = log, log
	require.NoError(t, worker.command.Start())
	go func() {
		worker.done <- worker.command.Wait()
		_ = log.Close()
	}()
	f.workers = append(f.workers, worker)
	t.Cleanup(func() { worker.kill(t) })
	if ready {
		f.ready(t, worker)
	}
	return worker
}

func (f *haProcessFixture) ready(t *testing.T, worker *haWorkerProcess) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-worker.done:
			worker.exited = true
			output, readErr := os.ReadFile(worker.log)
			require.NoError(t, readErr)
			t.Fatalf("owned Runner exited before readiness: %v; %s", err, output)
		default:
		}
		request, err := http.NewRequest(http.MethodGet, worker.url+"/v1/rollouts", nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+f.token)
		response, err := (&http.Client{Timeout: 200 * time.Millisecond}).Do(request)
		if err == nil {
			status := response.StatusCode
			require.NoError(t, response.Body.Close())
			if status == http.StatusOK {
				request.URL.Path = "/v1/catalog"
				catalog, err := http.DefaultClient.Do(request)
				require.NoError(t, err)
				defer func() { require.NoError(t, catalog.Body.Close()) }()
				require.Equal(t, http.StatusOK, catalog.StatusCode)
				var advertised struct {
					Site struct {
						ToolsDigest string `json:"tools_digest"`
					} `json:"site"`
				}
				require.NoError(t, json.NewDecoder(catalog.Body).Decode(&advertised))
				require.Equal(t, f.wire.config.Executor.CatalogDigest(), advertised.Site.ToolsDigest)
				return
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("owned Runner did not become ready")
}

func (worker *haWorkerProcess) kill(t *testing.T) {
	t.Helper()
	if worker.exited {
		return
	}
	_ = worker.command.Process.Kill()
	select {
	case err := <-worker.done:
		worker.exited = true
		if err != nil {
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit)
		}
	case <-time.After(5 * time.Second):
		t.Error("owned Runner did not exit after SIGKILL")
	}
}

func (f *haProcessFixture) command(t *testing.T, strategy string) rollout.Command {
	t.Helper()
	command := f.wire.command
	// Every slot and its authenticated transport share this pool UUID.
	command.Version, command.RunnerUUID, command.Request.RunnerUUID = rollout.NativePipelineVersion, f.runnerUUID, f.runnerUUID
	command.ExecutionLimits = &rollout.ExecutionLimits{PlanSeconds: 1800, ApplySeconds: 7200, TaskSeconds: 1800}
	command.ID, command.EnvironmentID, command.Strategy = uuid.NewString(), uuid.NewString(), strategy
	command.Request.ID, command.Request.EnvironmentID = command.ID, command.EnvironmentID
	command.ToolsDigest = f.wire.config.Executor.CatalogDigest()
	command.Request.DeploymentToken, command.Request.MetadataKey = "", ""
	// Standalone local Engine authority does not claim Core registration support.
	command.NativeTarget = &rollout.NativeExecutionTarget{RunnerID: command.RunnerID, RunnerUUID: command.RunnerUUID, RuntimeVersion: rollout.NativeRuntimeVersion, PipelineVersion: rollout.NativePipelineVersion, ToolsDigest: command.ToolsDigest, StateBackendType: command.Request.Backend, StateConfigurationDigest: strings.Repeat("b", 64)}
	if strategy != "reviewed" {
		command.Approval = rollout.Approval{}
	}
	state := filepath.Join(f.root, command.ID+".tfstate")
	main := fmt.Sprintf("terraform {\n backend \"local\" { path = %q }\n}\nresource \"terraform_data\" \"marker\" { input = %q }\noutput \"value\" { value = terraform_data.marker.output }\noutput \"platform_orchestrator_metadata\" { value = { node = { installed = true } } }\n", state, command.ID)
	command.Request.Bundle = transportBundle(t, main)
	command.Request.BundleSHA256 = transportDigest(command.Request.Bundle)
	return command
}

func (f *haProcessFixture) publish(t *testing.T, command rollout.Command) {
	t.Helper()
	payload, err := json.Marshal(command)
	require.NoError(t, err)
	signed := rollout.SignedCommand{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(f.wire.key, payload))}
	payload, err = json.Marshal(signed)
	require.NoError(t, err)
	transportID, err := hmessaging.NativeRunnerToken(command.RunnerUUID)
	require.NoError(t, err)
	envelope := hmessaging.CommandEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "pipeline:" + command.ID,
		OrganizationID: command.OrgID, RunnerID: transportID, DeploymentID: command.ID, Type: CommandType,
		CreatedAt: time.Now().UTC(), ExpiresAt: command.ExpiresAt, Payload: payload}
	payload, err = json.Marshal(envelope)
	require.NoError(t, err)
	publisher, err := natstransport.NewPublisher(f.wire.connection)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerCommandSubject(command.OrgID, transportID)
	require.NoError(t, err)
	messageID, err := hmessaging.NativeMessageID(command.OrgID, transportID, "commands", envelope.Type, envelope.CommandID)
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(t.Context(), hmessaging.Message{ID: messageID, Subject: subject, Data: payload, CreatedAt: envelope.CreatedAt, ExpiresAt: envelope.ExpiresAt}))
}

func (f *haProcessFixture) table(name string) string {
	return pq.QuoteIdentifier(f.pg.ApplicationSchema) + "." + pq.QuoteIdentifier(name)
}

func (f *haProcessFixture) progress(t *testing.T, id, phase string) rollout.Progress {
	t.Helper()
	var progress rollout.Progress
	ready := false
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		var body []byte
		err := f.db.QueryRow(`SELECT progress FROM `+f.table("rollout_commands")+` WHERE id=$1`, id).Scan(&body)
		if err == nil && json.Unmarshal(body, &progress) == nil && progress.Phase == phase {
			ready = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		f.retainFailure(t)
	}
	require.True(t, ready, "wanted phase %s; last public progress phase=%s code=%s revision=%d", phase, progress.Phase, progress.ErrorCode, progress.Revision)
	return progress
}

func (f *haProcessFixture) retainFailure(t *testing.T) {
	t.Helper()
	if f.artifacts != "" {
		return
	}
	directory, err := os.MkdirTemp("", "stellwerk-ha-process-failure-")
	require.NoError(t, err)
	f.artifacts = directory
	rows, err := f.db.Query(`SELECT json_build_object('id',id,'progress',convert_from(progress,'UTF8')::json,'worker',owner_worker,'generation',owner_generation)::text FROM ` + f.table("rollout_commands"))
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var progress []json.RawMessage
	for rows.Next() {
		var data string
		require.NoError(t, rows.Scan(&data))
		progress = append(progress, json.RawMessage(data))
	}
	require.NoError(t, rows.Err())
	data, err := json.MarshalIndent(progress, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "progress.json"), data, 0600))
	statuses := f.workerStatuses(t)
	data, err = json.MarshalIndent(statuses, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "workers.json"), data, 0600))
	require.NoError(t, filepath.WalkDir(f.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || entry.Name() != "process.log" {
			return walkErr
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(filepath.Join(directory, filepath.Base(filepath.Dir(path))+".log"), body, 0600)
	}))
	t.Logf("Private failed-process diagnostics retained at %s", directory)
}

func (f *haProcessFixture) workerStatuses(t *testing.T) []map[string]any {
	t.Helper()
	var statuses []map[string]any
	for _, worker := range f.workers {
		status := map[string]any{"url": worker.url, "pid": worker.command.Process.Pid, "exited": worker.exited}
		select {
		case err := <-worker.done:
			worker.exited = true
			status["exited"], status["exit"] = true, fmt.Sprint(err)
		default:
		}
		request, err := http.NewRequest(http.MethodGet, worker.url+"/v1/catalog", nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+f.token)
		response, err := (&http.Client{Timeout: 300 * time.Millisecond}).Do(request)
		if err == nil {
			status["http"] = response.StatusCode
			require.NoError(t, response.Body.Close())
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func (f *haProcessFixture) action(t *testing.T, worker *haWorkerProcess, id, action, subject string) {
	t.Helper()
	f.operatorAction(t, worker, id, action, map[string]string{"subject": subject})
}

func (f *haProcessFixture) operatorAction(t *testing.T, worker *haWorkerProcess, id, action string, details map[string]string) {
	t.Helper()
	details["actor"] = "owned-ha-operator"
	body, err := json.Marshal(details)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, worker.url+"/v1/rollouts/"+id+"/"+action, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+f.token)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, "operator action: %s", data)
}

func (f *haProcessFixture) owner(t *testing.T, id string) (string, int64) {
	t.Helper()
	var worker string
	var generation int64
	require.NoError(t, f.db.QueryRow(`SELECT owner_worker,owner_generation FROM `+f.table("rollout_commands")+` WHERE id=$1`, id).Scan(&worker, &generation))
	return worker, generation
}

func (f *haProcessFixture) count(t *testing.T, table, id string) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM `+f.table(table)+` WHERE rollout=$1`, id).Scan(&count))
	return count
}

func haSignal(t *testing.T, process *haWorkerProcess, signal syscall.Signal) {
	t.Helper()
	require.NoError(t, process.command.Process.Signal(signal))
}

func haWaitFile(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool { _, err := os.Stat(path); return err == nil }, 20*time.Second, 20*time.Millisecond)
}

func haRollback(t *testing.T, tx *sql.Tx) {
	t.Helper()
	err := tx.Rollback()
	require.True(t, err == nil || err == sql.ErrTxDone)
}

func (f *haProcessFixture) stateMarker(t *testing.T, id string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(f.root, id+".tfstate"))
	require.NoError(t, err)
	var state struct {
		Resources []struct {
			Type      string
			Instances []struct{ Attributes map[string]any }
		}
	}
	require.NoError(t, json.Unmarshal(body, &state))
	require.Len(t, state.Resources, 1)
	require.Equal(t, "terraform_data", state.Resources[0].Type)
	require.Len(t, state.Resources[0].Instances, 1)
	input, ok := state.Resources[0].Instances[0].Attributes["input"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, id, input["value"])
}

// The fixture operator issues authority outside each worker's journal/scratch.
func (f *haProcessFixture) activation(t *testing.T, pg rollout.PostgresConfig) string {
	t.Helper()
	dsn, err := os.ReadFile(pg.DSNFile)
	require.NoError(t, err)
	key, err := os.ReadFile(pg.IdentityFile)
	require.NoError(t, err)
	grant := map[string]any{"version": 1, "activation_id": strings.ReplaceAll(uuid.NewString(), "-", ""), "state": "active", "reconciliation": "fresh",
		"org_id": f.wire.config.OrgID, "runner_uuid": f.runnerUUID, "application_schema": pg.ApplicationSchema, "system_schema": pg.SystemSchema,
		"runtime_version": rollout.NativeRuntimeVersion, "dsn_sha256": fmt.Sprintf("%x", sha256.Sum256(dsn)), "identity_sha256": fmt.Sprintf("%x", sha256.Sum256(key)),
		"catalogue_sha256": f.wire.config.Executor.CatalogDigest()}
	body, err := json.Marshal(grant)
	require.NoError(t, err)
	path := filepath.Join(f.root, "activation-"+uuid.NewString()+".json")
	require.NoError(t, os.WriteFile(path, body, 0400))
	return path
}
