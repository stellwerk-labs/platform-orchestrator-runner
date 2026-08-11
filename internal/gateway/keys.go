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
)

type PublicKeyResolver interface {
	Resolve(ctx context.Context, organizationID, runnerID string) ([]byte, error)
}

type StaticPublicKeyResolver struct {
	OrganizationID string
	RunnerID       string
	PublicKey      []byte
}

func (r StaticPublicKeyResolver) Resolve(_ context.Context, organizationID, runnerID string) ([]byte, error) {
	if organizationID != r.OrganizationID || runnerID != r.RunnerID || len(r.PublicKey) == 0 {
		return nil, errors.New("runner public key is not configured")
	}
	return append([]byte(nil), r.PublicKey...), nil
}

type controlPlanePublicKeyResolver struct {
	baseURL string
	client  *http.Client
	ttl     time.Duration

	mu    sync.Mutex
	cache map[string]cachedPublicKey
}

type cachedPublicKey struct {
	key       []byte
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

func NewStaticPublicKeyResolver(organizationID, runnerID, publicKeyFile string) (PublicKeyResolver, error) {
	key, err := os.ReadFile(publicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read static runner public key: %w", err)
	}
	return StaticPublicKeyResolver{OrganizationID: organizationID, RunnerID: runnerID, PublicKey: key}, nil
}

func (r *controlPlanePublicKeyResolver) Resolve(ctx context.Context, organizationID, runnerID string) ([]byte, error) {
	cacheKey := organizationID + "\x00" + runnerID
	now := time.Now()
	r.mu.Lock()
	if cached, ok := r.cache[cacheKey]; ok && now.Before(cached.expiresAt) {
		key := append([]byte(nil), cached.key...)
		r.mu.Unlock()
		return key, nil
	}
	r.mu.Unlock()

	endpoint := r.baseURL + "/internal/orgs/" + url.PathEscape(organizationID) + "/runners/" + url.PathEscape(runnerID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch runner public key: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("fetch runner public key: control plane returned %s", response.Status)
	}
	var body struct {
		RunnerConfiguration json.RawMessage `json:"runner_configuration"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("decode runner: %w", err)
	}
	var runnerConfig struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(body.RunnerConfiguration, &runnerConfig); err != nil {
		return nil, fmt.Errorf("decode runner configuration: %w", err)
	}
	if runnerConfig.Type != "kubernetes-agent" || runnerConfig.Key == "" {
		return nil, errors.New("runner is not a Kubernetes agent with a public key")
	}
	key := []byte(runnerConfig.Key)
	r.mu.Lock()
	r.cache[cacheKey] = cachedPublicKey{key: append([]byte(nil), key...), expiresAt: now.Add(r.ttl)}
	r.mu.Unlock()
	return key, nil
}
