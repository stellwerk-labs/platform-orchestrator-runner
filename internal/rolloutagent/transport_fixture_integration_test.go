//go:build integration

package rolloutagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/golib/hnats"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gateway"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	steps "github.com/stellwerk-labs/platform-orchestrator-runner/internal/pipelinesteps"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

type transportFixture struct {
	engine                *rollout.Engine
	config                rollout.Config
	client                *gatewayapi.Client
	clientConfig          gatewayapi.ClientConfig
	connection            *nats.Conn
	js                    nats.JetStreamContext
	key                   ed25519.PrivateKey
	command               rollout.Command
	trace                 string
	dropBefore, dropAfter atomic.Value
	lastAck               atomic.Value
	clockOffset           atomic.Int64
	legacyResults         atomic.Int64
}

func newTransportFixture(t *testing.T, strategy string) *transportFixture {
	return newTransportFixtureForUUID(t, strategy, "")
}

func newTransportFixtureForUUID(t *testing.T, strategy, runnerUUID string) *transportFixture {
	t.Helper()
	url := os.Getenv("AGENT_DELIVERY_NATS_URL")
	require.NotEmpty(t, url, "run make test-agent-delivery-integration with a disposable broker")
	require.Equal(t, "1", os.Getenv("AGENT_DELIVERY_ISOLATED"), "broker must belong to this test run")
	connection, err := nats.Connect(url, nats.Timeout(2*time.Second), nats.RetryOnFailedConnect(true), nats.MaxReconnects(100), nats.ReconnectWait(100*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(connection.Close)
	require.Eventually(t, connection.IsConnected, 10*time.Second, 100*time.Millisecond)
	modern, err := hnats.NewJetStream(connection)
	require.NoError(t, err)
	require.NoError(t, hnats.EnsureStandardStreams(t.Context(), modern, 1))
	js, err := connection.JetStream()
	require.NoError(t, err)
	for _, bucket := range []string{"PO_RUNNER_BUNDLES", "PO_RUNNER_LOGS"} {
		if _, err := js.ObjectStore(bucket); err != nil {
			_, err = js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: bucket, Storage: nats.FileStorage})
			require.NoError(t, err)
		}
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	f := &transportFixture{connection: connection, js: js, key: key}
	f.dropBefore.Store("")
	f.dropAfter.Store("")
	f.lastAck.Store(gatewayapi.ReceiptRequest{})
	f.config = rollout.Config{Directory: filepath.Join(t.TempDir(), "site"), OrgID: "wire-" + uuid.NewString(), RunnerID: "site", PublicKey: public, Executor: &rolloutexec.Executor{}}
	f.engine, err = rollout.Open(t.Context(), f.config)
	require.NoError(t, err)
	t.Cleanup(func() { f.closeEngine(t) })
	backend, err := gateway.NewNATSBackend(connection)
	require.NoError(t, err)
	agentPublic, agentPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(agentPublic)
	require.NoError(t, err)
	server, err := gateway.NewServer(gateway.ServerConfig{Backend: backend,
		PublicKeys: gateway.StaticPublicKeyResolver{OrganizationID: f.config.OrgID, RunnerID: f.config.RunnerID, RunnerUUID: runnerUUID, PublicKey: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})},
		ReceiptKey: make([]byte, 32), RunnerTokenSalt: "isolated-wire-fixture", FetchWait: 2 * time.Second,
		Now: func() time.Time { return time.Now().Add(time.Duration(f.clockOffset.Load())).UTC() }})
	require.NoError(t, err)
	httpServer := httptest.NewUnstartedServer(f.transportFaultHandler(t, server.Handler()))
	httpServer.EnableHTTP2 = false
	httpServer.StartTLS()
	t.Cleanup(httpServer.Close)
	ca := filepath.Join(t.TempDir(), "gateway-ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: httpServer.Certificate().Raw}), 0600))
	privateDER, err := x509.MarshalPKCS8PrivateKey(agentPrivate)
	require.NoError(t, err)
	f.clientConfig = gatewayapi.ClientConfig{BaseURL: httpServer.URL, OrganizationID: f.config.OrgID, RunnerID: f.config.RunnerID, RunnerUUID: runnerUUID, CAFile: ca,
		PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), Timeout: 10 * time.Second}
	f.client, err = gatewayapi.NewClient(f.clientConfig)
	require.NoError(t, err)
	f.initializeCommand(t, strategy)
	return f
}

// Buffer the actual Gateway response after its real backend work, then lose
// the connection. This injects transport loss, never a fabricated domain reply.
func (f *transportFixture) transportFaultHandler(t *testing.T, handler http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
		if err != nil {
			t.Error(err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		kind := filepath.Base(r.URL.Path)
		if kind == "events" {
			var event hmessaging.EventEnvelope
			_ = json.Unmarshal(data, &event)
			kind = event.Type
		}
		if kind != "" && f.dropBefore.CompareAndSwap(kind, "") {
			if kind == "ack" {
				var ack gatewayapi.ReceiptRequest
				_ = json.Unmarshal(data, &ack)
				f.lastAck.Store(ack)
			}

			closeTransport(t, w)
			return
		}
		if kind == "results" {
			f.legacyResults.Add(1)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		if kind != "" && response.Code >= 200 && response.Code < 300 && f.dropAfter.CompareAndSwap(kind, "") {
			closeTransport(t, w)
			return
		}
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	})
}

func closeTransport(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	connection, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_ = connection.Close()
}

func (f *transportFixture) initializeCommand(t *testing.T, strategy string) {
	t.Helper()
	binary, err := exec.LookPath("tofu")
	require.NoError(t, err, "real OpenTofu is required")
	bin := t.TempDir()
	f.trace = filepath.Join(bin, "trace")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(f.trace) + "\nexec " + quote(binary) + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "tofu"), []byte(script), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	id, environment := uuid.NewString(), uuid.NewString()
	main := fmt.Sprintf("terraform {\n backend \"local\" { path = %q }\n}\nresource \"terraform_data\" \"marker\" { input = \"PRIVATE_AGENT_VALUE\" }\noutput \"value\" { value = terraform_data.marker.output }\noutput \"platform_orchestrator_metadata\" { value = { node = { installed = true } } }\n", filepath.Join(t.TempDir(), "terraform.tfstate"))
	bundle := transportBundle(t, main)
	f.command = rollout.Command{Version: rollout.PipelineVersion, Strategy: strategy, ID: id, OrgID: f.config.OrgID, RunnerID: f.config.RunnerID, EnvironmentID: environment,
		IssuedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(15 * time.Minute), ToolsDigest: f.config.Executor.CatalogDigest(),
		Request: rolloutexec.Request{ID: id, OrgID: f.config.OrgID, RunnerID: f.config.RunnerID, EnvironmentID: environment, Backend: "local", Tool: "opentofu", Mode: "deploy",
			Bundle: bundle, BundleSHA256: transportDigest(bundle), DeploymentToken: "PRIVATE_AGENT_DEPLOYMENT_TOKEN", EncryptedOutputsRecipient: identity.Recipient().String(), Manifest: json.RawMessage(`{}`)}}
	if strategy == "reviewed" {
		f.command.Approval = rollout.Approval{Mode: "local"}
	}
}

func transportBundle(t *testing.T, main string) []byte {
	t.Helper()
	var data bytes.Buffer
	gzipWriter := gzip.NewWriter(&data)
	archive := tar.NewWriter(gzipWriter)
	require.NoError(t, archive.WriteHeader(&tar.Header{Name: "main.tf", Mode: 0600, Size: int64(len(main))}))
	_, err := archive.Write([]byte(main))
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, gzipWriter.Close())
	return data.Bytes()
}

func transportDigest(data []byte) string {
	value := sha256.Sum256(data)
	return hex.EncodeToString(value[:])
}

func (f *transportFixture) signed(t *testing.T) rollout.SignedCommand {
	t.Helper()
	data, err := json.Marshal(f.command)
	require.NoError(t, err)
	return rollout.SignedCommand{Payload: data, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(f.key, data))}
}

func (f *transportFixture) deliver(t *testing.T, signed rollout.SignedCommand) gatewayapi.CommandResponse {
	t.Helper()
	body, err := json.Marshal(signed)
	require.NoError(t, err)
	envelope := hmessaging.CommandEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "pipeline:" + f.command.ID,
		OrganizationID: f.config.OrgID, RunnerID: f.config.RunnerID, DeploymentID: f.command.ID, Type: CommandType,
		CreatedAt: time.Now().UTC(), ExpiresAt: f.command.ExpiresAt, Payload: body}
	body, err = json.Marshal(envelope)
	require.NoError(t, err)
	publisher, err := natstransport.NewPublisher(f.connection)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerCommandSubject(f.config.OrgID, f.config.RunnerID)
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(t.Context(), hmessaging.Message{ID: envelope.CommandID, Subject: subject, Data: body, CreatedAt: envelope.CreatedAt, ExpiresAt: envelope.ExpiresAt}))
	delivery, err := f.client.NextCommand(t.Context())
	require.NoError(t, err)
	return delivery
}

func (f *transportFixture) wait(t *testing.T, phase string) rollout.Progress {
	t.Helper()
	var progress rollout.Progress
	require.Eventually(t, func() bool {
		var err error
		progress, err = f.engine.Get(t.Context(), f.command.ID)
		return err == nil && progress.Phase == phase
	}, 20*time.Second, 20*time.Millisecond, "pipeline did not reach %s", phase)
	return progress
}

func (f *transportFixture) closeEngine(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.engine.Close(ctx))
}

func (f *transportFixture) operations(t *testing.T, wanted string) int {
	t.Helper()
	data, err := os.ReadFile(f.trace)
	require.NoError(t, err)
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[1] == wanted {
			count++
		}
	}
	return count
}

func (f *transportFixture) addReceiptTask(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	require.NoError(t, err, "the existing real fixed-tool fixture requires Python3")
	python, err = filepath.EvalSymlinks(python)
	require.NoError(t, err)
	receipt := filepath.Join(t.TempDir(), "receipt")
	body := fmt.Sprintf(`import json,sys,pathlib
r=json.load(sys.stdin)
p=pathlib.Path(%q)
if not p.exists():
 p.write_text(r['effect_id'])
 sys.exit(1)
assert p.read_text()==r['effect_id']
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, receipt)
	program := filepath.Join(t.TempDir(), "task.py")
	require.NoError(t, os.WriteFile(program, []byte(body), 0600))
	interpreter, err := os.ReadFile(python)
	require.NoError(t, err)
	f.config.Executor.Tools = []rolloutexec.Tool{{ID: "receipt", Name: "receipt", Phase: "post_apply", Retry: "receipt", Definition: steps.Definition{
		ProtocolVersion: 1, Name: "receipt", Version: "1", Phase: "post_apply", Retry: "receipt", Executable: program, ExecutableSHA256: transportDigest([]byte(body)),
		Interpreter: python, InterpreterSHA256: transportDigest(interpreter)}}}
	f.command.Steps = []rolloutexec.Step{{ID: "post", ToolID: "receipt", Parameters: json.RawMessage(`{}`)}}
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
	return receipt
}
