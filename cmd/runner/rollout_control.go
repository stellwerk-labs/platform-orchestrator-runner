package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
)

var rolloutControlWait bool
var rolloutControlTimeout time.Duration

func runRolloutMode(ctx context.Context, mode string) error {
	if mode == RolloutMode {
		return runRollout(ctx)
	}
	if rolloutControlTimeout <= 0 {
		return errors.New("local operator deadline must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, rolloutControlTimeout)
	defer cancel()
	cfg, _, err := loadRolloutConfig()
	if err != nil {
		return err
	}
	credential, err := os.ReadFile(cfg.APITokenFile)
	if err != nil || len(strings.TrimSpace(string(credential))) < 32 {
		return errors.New("read local operator credential")
	}
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	status, err := requestWorkerControl(ctx, client, cfg.Listen, strings.TrimSpace(string(credential)), mode)
	if err == nil && mode == "rollout-drain" && rolloutControlWait {
		status, err = waitWorkerDrain(ctx, client, cfg.Listen, strings.TrimSpace(string(credential)), status)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}

func requestWorkerControl(ctx context.Context, client *http.Client, address, token, mode string) (rollout.WorkerStatus, error) {
	method, endpoint := http.MethodGet, "status"
	switch mode {
	case "rollout-ready":
		endpoint = "ready"
	case "rollout-drain":
		method, endpoint = http.MethodPost, "drain"
	}
	var status rollout.WorkerStatus
	request, err := http.NewRequestWithContext(ctx, method, "http://"+address+"/v1/worker/"+endpoint, nil)
	if err != nil {
		return status, errors.New("invalid local operator endpoint")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return status, errors.New("local operator request incomplete")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || json.NewDecoder(http.MaxBytesReader(nil, response.Body, 4096)).Decode(&status) != nil || !status.AuthorityAlive {
		return status, errors.New("local worker unavailable")
	}
	return status, nil
}

func waitWorkerDrain(ctx context.Context, client *http.Client, address, token string, status rollout.WorkerStatus) (rollout.WorkerStatus, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !status.Draining || status.Accepting {
			return status, errors.New("drain incomplete; worker admission is not closed")
		}
		if status.ActiveStages == 0 {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, errors.New("drain incomplete; worker remains draining")
		case <-ticker.C:
		}
		var err error
		status, err = requestWorkerControl(ctx, client, address, token, "rollout-status")
		if err != nil {
			return status, err
		}
	}
}
