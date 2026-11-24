package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hnats"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
)

func loadConfig() (config, error) {
	var cfg config
	file, err := os.Open(os.Getenv("ROLLOUT_LAB_CONFIG"))
	if err != nil {
		return cfg, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&cfg); err != nil {
		return cfg, err
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return cfg, errors.New("fixture must bind loopback")
	}
	for _, dir := range []string{"imports", "projections"} {
		if err = os.MkdirAll(filepath.Join(cfg.Directory, dir), 0700); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}
func newLab(cfg config, publisher *natstransport.Publisher) (*lab, error) {
	raw, err := os.ReadFile(cfg.SigningKeyFile)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid fixture signer")
	}
	token, err := os.ReadFile(cfg.OperatorTokenFile)
	if err != nil {
		return nil, err
	}
	state := &lab{cfg: cfg, publisher: publisher, key: key, token: strings.TrimSpace(string(token))}
	secret, err := os.ReadFile(cfg.APITokenFile)
	if err != nil {
		return nil, err
	}
	state.apiToken = strings.TrimSpace(string(secret))
	if len(state.apiToken) < 32 {
		return nil, errors.New("adapter token too short")
	}
	return state, nil
}
func connectBus(ctx context.Context, cfg config) (*nats.Conn, error) {
	connection, err := natstransport.Connect(natstransport.Config{URL: cfg.NATSURL, Token: cfg.NATSToken, Name: "dbos-site-fixture"})
	if err != nil {
		return nil, err
	}
	js, err := hnats.NewJetStream(connection)
	if err == nil {
		err = hnats.EnsureStandardStreams(ctx, js, 1)
	}
	if err != nil {
		connection.Close()
		return nil, err
	}
	return connection, nil
}
