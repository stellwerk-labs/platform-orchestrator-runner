package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
)

type RunnerAuthority struct {
	PublicKey  []byte
	RunnerUUID string
}

type PublicKeyResolver interface {
	Resolve(ctx context.Context, organizationID, runnerID string) (RunnerAuthority, error)
}

type StaticPublicKeyResolver struct {
	OrganizationID string
	RunnerID       string
	PublicKey      []byte
	RunnerUUID     string
}

func (r StaticPublicKeyResolver) Resolve(_ context.Context, organizationID, runnerID string) (RunnerAuthority, error) {
	if organizationID != r.OrganizationID || runnerID != r.RunnerID || strings.HasPrefix(runnerID, hmessaging.NativeRunnerTokenPrefix) || len(r.PublicKey) == 0 {
		return RunnerAuthority{}, errors.New("runner authority is not configured")
	}
	if r.RunnerUUID != "" {
		if _, err := hmessaging.NativeRunnerToken(r.RunnerUUID); err != nil {
			return RunnerAuthority{}, err
		}
	}
	return RunnerAuthority{PublicKey: append([]byte(nil), r.PublicKey...), RunnerUUID: r.RunnerUUID}, nil
}

type controlPlanePublicKeyResolver struct {
	baseURL string
	client  *http.Client
	ttl     time.Duration

	mu    sync.Mutex
	cache map[string]cachedPublicKey
}

type cachedPublicKey struct {
	authority RunnerAuthority
	expiresAt time.Time
}

func NewControlPlanePublicKeyResolver(baseURL string, client *http.Client, ttl time.Duration) (PublicKeyResolver, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("control-plane URL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("control-plane URL must use HTTP or HTTPS")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &controlPlanePublicKeyResolver{baseURL: parsed.String(), client: client, ttl: ttl, cache: map[string]cachedPublicKey{}}, nil
}

func NewStaticPublicKeyResolver(organizationID, runnerID, publicKeyFile, runnerUUID string) (PublicKeyResolver, error) {
	key, err := os.ReadFile(publicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read static runner public key: %w", err)
	}
	resolver := StaticPublicKeyResolver{OrganizationID: organizationID, RunnerID: runnerID, PublicKey: key, RunnerUUID: runnerUUID}
	if _, err := resolver.Resolve(context.Background(), organizationID, runnerID); err != nil {
		return nil, err
	}
	return resolver, nil
}

func (r *controlPlanePublicKeyResolver) Resolve(ctx context.Context, organizationID, runnerID string) (RunnerAuthority, error) {
	if strings.HasPrefix(runnerID, hmessaging.NativeRunnerTokenPrefix) {
		return RunnerAuthority{}, errors.New("runner domain identity is invalid")
	}
	cacheKey := organizationID + "\x00" + runnerID
	now := time.Now()
	r.mu.Lock()
	if cached, ok := r.cache[cacheKey]; ok && now.Before(cached.expiresAt) {
		authority := cached.authority
		authority.PublicKey = append([]byte(nil), authority.PublicKey...)
		r.mu.Unlock()
		return authority, nil
	}
	r.mu.Unlock()

	endpoint := r.baseURL + "/internal/orgs/" + url.PathEscape(organizationID) + "/runners/" + url.PathEscape(runnerID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RunnerAuthority{}, err
	}
	response, err := r.client.Do(request)
	if err != nil {
		return RunnerAuthority{}, fmt.Errorf("fetch runner public key: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return RunnerAuthority{}, fmt.Errorf("fetch runner public key: control plane returned %s", response.Status)
	}
	var body struct {
		RunnerConfiguration json.RawMessage `json:"runner_configuration"`
		UUID                string          `json:"uuid"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil {
		return RunnerAuthority{}, fmt.Errorf("decode runner: %w", err)
	}
	var runnerConfig struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(body.RunnerConfiguration, &runnerConfig); err != nil {
		return RunnerAuthority{}, fmt.Errorf("decode runner configuration: %w", err)
	}
	authority := RunnerAuthority{}
	switch runnerConfig.Type {
	case "native-agent":
		if _, err := hmessaging.NativeRunnerToken(body.UUID); err != nil {
			return RunnerAuthority{}, errors.New("native runner authority is invalid")
		}
		authority.RunnerUUID = body.UUID
		authority.PublicKey = []byte(runnerConfig.Key)
	case "kubernetes-agent":
		authority.PublicKey = []byte(runnerConfig.Key)
	case "kubernetes", "kubernetes-eks", "kubernetes-gke", "serverless-ecs":
	default:
		return RunnerAuthority{}, errors.New("runner authority type is unsupported")
	}
	cached := authority
	cached.PublicKey = append([]byte(nil), authority.PublicKey...)
	r.mu.Lock()
	r.cache[cacheKey] = cachedPublicKey{authority: cached, expiresAt: now.Add(r.ttl)}
	r.mu.Unlock()
	return authority, nil
}
