// rollout-lab is the loopback-only submission/projection adapter for the isolated experiment.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutagent"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

type config struct {
	Directory         string             `json:"directory"`
	Listen            string             `json:"listen"`
	NATSURL           string             `json:"nats_url"`
	NATSToken         string             `json:"nats_token"`
	OrgID             string             `json:"org_id"`
	RunnerID          string             `json:"runner_id"`
	SigningKeyFile    string             `json:"signing_key_file"`
	APITokenFile      string             `json:"api_token_file"`
	OperatorTokenFile string             `json:"operator_token_file"`
	OperatorURL       string             `json:"operator_url"`
	Kubeconfig        string             `json:"kubeconfig"`
	StateNamespace    string             `json:"state_namespace"`
	Tools             []rolloutexec.Tool `json:"tools"`
	Capabilities      []string           `json:"capabilities"`
}

type lab struct {
	cfg       config
	publisher *natstransport.Publisher
	key       ed25519.PrivateKey
	token     string
	apiToken  string
	mu        sync.Mutex
}

type draft struct {
	EnvironmentID string   `json:"environment_id"`
	Value         string   `json:"value"`
	ApprovalMode  string   `json:"approval_mode"`
	Pre           []string `json:"pre_steps"`
	Checks        []string `json:"check_steps"`
	Post          []string `json:"post_steps"`
}

type submission struct {
	Digest   string                     `json:"digest"`
	Envelope hmessaging.CommandEnvelope `json:"envelope"`
	Initial  rollout.Progress           `json:"initial"`
}
type projection struct {
	rollout.Progress
	LastReceivedAt time.Time `json:"last_received_at"`
}

var envPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

func main() {
	start := run
	if len(os.Args) == 2 && os.Args[1] == "gateway" {
		start = runFixtureGateway
	}
	if err := start(); err != nil {
		log.Print("rollout fixture stopped: ", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	connection, err := connectBus(ctx, cfg)
	if err != nil {
		return err
	}
	defer connection.Close()
	publisher, err := natstransport.NewPublisher(connection)
	if err != nil {
		return err
	}
	state, err := newLab(cfg, publisher)
	if err != nil {
		return err
	}
	go state.project(ctx, connection)
	go state.projectRefusals(ctx, connection)
	server := &http.Server{Addr: cfg.Listen, Handler: state.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (l *lab) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runner-rollouts/catalog", l.catalog)
	mux.HandleFunc("GET /api/runner-rollouts", l.list)
	mux.HandleFunc("POST /api/runner-rollouts", l.create)
	mux.HandleFunc("GET /api/runner-rollouts/{id}", l.get)
	mux.HandleFunc("GET /api/runner-rollouts/{id}/plan-logs", l.planLogs)
	mux.HandleFunc("GET /api/runner-rollouts/{id}/execution-logs", l.executionLogs)
	mux.HandleFunc("POST /api/runner-rollouts/{id}/{action}", l.action)
	mux.HandleFunc("/", l.serveConsole)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if l.acceptRequest(w, r) {
			mux.ServeHTTP(w, r)
		}
	})
}

func (l *lab) catalog(w http.ResponseWriter, r *http.Request) {
	tools := make([]map[string]any, 0, len(l.cfg.Tools))
	for _, tool := range l.cfg.Tools {
		reason := l.incompatible(tool)
		phase := map[string]string{"pre_plan": "pre", "plan_check": "check", "post_apply": "post"}[tool.Phase]
		tools = append(tools, map[string]any{"id": tool.ID, "name": tool.Name, "phase": phase, "required_capabilities": tool.RequiredCapabilities, "compatible": reason == "", "reason": reason})
	}
	reply(w, 200, map[string]any{"site": map[string]any{"org_id": l.cfg.OrgID, "runner_id": l.cfg.RunnerID, "capabilities": l.cfg.Capabilities}, "tools": tools})
}

func (l *lab) list(w http.ResponseWriter, r *http.Request) {
	files, _ := filepath.Glob(filepath.Join(l.cfg.Directory, "imports", "*.json"))
	values := make([]projection, 0, len(files))
	for _, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".json")
		if p, err := l.read(id); err == nil {
			values = append(values, p)
		}
	}
	slices.SortFunc(values, func(a, b projection) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	reply(w, 200, values)
}

func (l *lab) read(id string) (projection, error) {
	if _, err := uuid.Parse(id); err != nil {
		return projection{}, err
	}
	data, err := os.ReadFile(filepath.Join(l.cfg.Directory, "projections", id+".json"))
	if err == nil {
		var p projection
		err = json.Unmarshal(data, &p)
		return p, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return projection{}, err
	}
	var pending submission
	data, err = os.ReadFile(filepath.Join(l.cfg.Directory, "imports", id+".json"))
	if err == nil {
		err = json.Unmarshal(data, &pending)
	}
	return projection{Progress: pending.Initial}, err
}

func (l *lab) create(w http.ResponseWriter, r *http.Request) {
	var d draft
	if decode(w, r, &d) != nil || !validEnvironment(d.EnvironmentID) || len(d.Value) > 1024 || (d.ApprovalMode != "local" && d.ApprovalMode != "policy") {
		reply(w, 400, map[string]string{"error": "invalid_rollout"})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) == 0 || len(key) > 128 {
		reply(w, 400, map[string]string{"error": "idempotency_key_required"})
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	frozen, err := l.prepare(d, key)
	if err != nil {
		reply(w, 409, map[string]string{"error": "rollout_request_conflict"})
		return
	}
	bytes, _ := json.Marshal(frozen.Envelope)
	subject, _ := hmessaging.RunnerCommandSubject(l.cfg.OrgID, l.cfg.RunnerID)
	if l.publisher.Publish(r.Context(), hmessaging.Message{ID: frozen.Envelope.CommandID, Subject: subject, Data: bytes, CreatedAt: frozen.Envelope.CreatedAt}) != nil {
		reply(w, 503, map[string]string{"error": "handoff_unavailable"})
		return
	}
	if p, err := l.read(frozen.Initial.ID); err == nil {
		reply(w, 202, p)
	} else {
		reply(w, 202, frozen.Initial)
	}
}
func (l *lab) prepare(d draft, key string) (submission, error) {
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(l.cfg.OrgID+"/"+l.cfg.RunnerID+"/"+key)).String()
	body, _ := json.Marshal(d)
	hash := sha256.Sum256(body)
	digest := hex.EncodeToString(hash[:])
	file := filepath.Join(l.cfg.Directory, "imports", id+".json")
	if saved, err := os.ReadFile(file); err == nil {
		var s submission
		if json.Unmarshal(saved, &s) != nil || s.Digest != digest {
			return s, errors.New("idempotency conflict")
		}
		return s, nil
	}
	executor := &rolloutexec.Executor{Tools: l.cfg.Tools, Capabilities: l.cfg.Capabilities}
	toolsDigest := executor.CatalogDigest()
	configuration := fmt.Sprintf("terraform {\n backend \"kubernetes\" {\n namespace = %q\n secret_suffix = %q\n config_path = %q\n }\n}\nresource \"terraform_data\" \"marker\" { input = %q }\noutput \"platform_orchestrator_metadata\" { value = {} }\noutput \"marker\" { value = terraform_data.marker.output }\n", l.cfg.StateNamespace, "dbos-"+d.EnvironmentID, l.cfg.Kubeconfig, d.Value)
	now := time.Now().UTC()
	cmd := rollout.Command{Version: rollout.Version, ID: id, OrgID: l.cfg.OrgID, RunnerID: l.cfg.RunnerID, EnvironmentID: d.EnvironmentID, IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour), ToolsDigest: toolsDigest, Approval: rollout.Approval{Mode: d.ApprovalMode, MaxAdd: 1, MaxChange: 1, MaxDestroy: 0}, Request: rolloutexec.Request{ID: id, OrgID: l.cfg.OrgID, RunnerID: l.cfg.RunnerID, EnvironmentID: d.EnvironmentID, Configuration: configuration, Backend: "kubernetes", Tool: "opentofu", Manifest: json.RawMessage(body)}}
	for _, ids := range [][]string{d.Pre, d.Checks, d.Post} {
		for _, tool := range ids {
			cmd.Steps = append(cmd.Steps, rolloutexec.Step{ID: tool, ToolID: tool, Parameters: json.RawMessage(`{}`)})
		}
	}
	if err := l.validateDraft(d); err != nil {
		return submission{}, err
	}
	payload, _ := json.Marshal(cmd)
	signed := rollout.SignedCommand{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(l.key, payload))}
	wire, _ := json.Marshal(signed)
	s := submission{Digest: digest, Initial: rollout.Progress{ID: id, EnvironmentID: d.EnvironmentID, Phase: "submitted", CreatedAt: now, Steps: []rollout.StepResult{}}, Envelope: hmessaging.CommandEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "rollout:" + id, OrganizationID: l.cfg.OrgID, RunnerID: l.cfg.RunnerID, DeploymentID: id, Type: rolloutagent.CommandType, CreatedAt: now, ExpiresAt: cmd.ExpiresAt, Payload: wire}}
	return s, atomicJSON(file, s)
}
func (l *lab) action(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		http.NotFound(w, r)
		return
	}
	action := r.PathValue("action")
	if action != "approve" && action != "repair" && action != "cancel" {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Subject string `json:"subject"`
		StepID  string `json:"step_id"`
	}
	if decode(w, r, &body) != nil {
		reply(w, 400, map[string]string{"error": "invalid_action"})
		return
	}
	data, _ := json.Marshal(map[string]string{"subject": body.Subject, "step_id": body.StepID, "actor": l.actor(r)})
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, l.cfg.OperatorURL+"/v1/rollouts/"+id+"/"+action, strings.NewReader(string(data)))
	if err != nil {
		reply(w, 503, map[string]string{"error": "local_operator_unavailable"})
		return
	}
	request.Header.Set("Authorization", "Bearer "+l.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		reply(w, 503, map[string]string{"error": "local_operator_unavailable"})
		return
	}
	defer func() { _ = response.Body.Close() }()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, 64<<10))
}
func (l *lab) project(ctx context.Context, connection *nats.Conn) {
	subject, _ := hmessaging.RunnerEventSubject(l.cfg.OrgID, l.cfg.RunnerID, rolloutagent.EventType)
	consumer, err := natstransport.NewBoundConsumer(connection, hmessaging.RunnerEventsStreamName, subject, "dbos-lab-projection")
	if err != nil {
		log.Print("projection unavailable")
		return
	}
	for ctx.Err() == nil {
		delivery, err := consumer.Fetch(ctx)
		if err != nil {
			continue
		}
		var envelope hmessaging.EventEnvelope
		var event rollout.Event
		if json.Unmarshal(delivery.Message.Data, &envelope) != nil || envelope.OrganizationID != l.cfg.OrgID || envelope.RunnerID != l.cfg.RunnerID || json.Unmarshal(envelope.Payload, &event) != nil {
			_ = delivery.Term()
			continue
		}
		if _, err := uuid.Parse(event.Progress.ID); err != nil {
			_ = delivery.Term()
			continue
		}
		l.mu.Lock()
		old, _ := l.read(event.Progress.ID)
		if event.Progress.Revision > old.Revision {
			err = atomicJSON(filepath.Join(l.cfg.Directory, "projections", event.Progress.ID+".json"), projection{Progress: event.Progress, LastReceivedAt: time.Now().UTC()})
		}
		l.mu.Unlock()
		if err == nil {
			_ = delivery.Ack()
		} else {
			_ = delivery.Nak(time.Second)
		}
	}
}
func decode(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}
func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func atomicJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".write-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err = temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

func (l *lab) validateDraft(d draft) error {
	tools := map[string]rolloutexec.Tool{}
	for _, tool := range l.cfg.Tools {
		tools[tool.ID] = tool
	}
	selected := map[string]string{}
	phases := []string{"pre_plan", "plan_check", "post_apply"}
	for i, ids := range [][]string{d.Pre, d.Checks, d.Post} {
		for _, id := range ids {
			if selected[id] != "" {
				return errors.New("duplicate task")
			}
			selected[id] = phases[i]
		}
	}
	for id, phase := range selected {
		tool, ok := tools[id]
		if !ok || tool.Phase != phase || l.incompatible(tool) != "" {
			return errors.New("task unavailable")
		}
	}
	return nil
}

func (l *lab) incompatible(tool rolloutexec.Tool) string {
	capabilities := append(append([]string{}, tool.RequiredCapabilities...), tool.Definition.Capabilities...)
	for _, required := range capabilities {
		if !slices.Contains(l.cfg.Capabilities, required) {
			return "Runner lacks " + required
		}
	}
	return ""
}
func (l *lab) get(w http.ResponseWriter, r *http.Request) {
	p, err := l.read(r.PathValue("id"))
	if err != nil {
		reply(w, 404, map[string]string{"error": "rollout_not_received"})
		return
	}
	reply(w, 200, p)
}
func (l *lab) serveConsole(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "http://127.0.0.1:38181/__fixture/login", http.StatusSeeOther)
}

func (l *lab) acceptRequest(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if l.apiToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+l.apiToken)) != 1 || l.actor(r) == "" {
			reply(w, http.StatusUnauthorized, map[string]string{"error": "adapter_authorization_required"})
			return false
		}
	}

	if r.Method != "POST" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin != "" && origin != "http://"+l.cfg.Listen {
		reply(w, 403, map[string]string{"error": "fixture_origin_refused"})
		return false
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		reply(w, 415, map[string]string{"error": "json_required"})
		return false
	}
	return true
}

func validEnvironment(id string) bool {
	parsed, err := uuid.Parse(id)
	return envPattern.MatchString(id) || (err == nil && parsed != uuid.Nil && parsed.String() == id)
}

func (l *lab) actor(r *http.Request) string {
	actor, err := uuid.Parse(r.Header.Get("X-Rollout-Actor"))
	if err != nil || actor == uuid.Nil || actor.String() == "ffffffff-ffff-ffff-ffff-ffffffffffff" {
		return ""
	}
	return actor.String()
}
