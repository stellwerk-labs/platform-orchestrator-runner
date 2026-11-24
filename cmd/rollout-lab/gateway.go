package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gateway"
)

func runFixtureGateway() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	local := filepath.Dir(cfg.SigningKeyFile)
	connection, err := connectBus(ctx, cfg)
	if err != nil {
		return err
	}
	defer connection.Close()
	if err = ensureObjectBuckets(connection); err != nil {
		return err
	}
	backend, err := gateway.NewNATSBackend(connection)
	if err != nil {
		return err
	}
	public, err := os.ReadFile(filepath.Join(local, "agent-public.pem"))
	if err != nil {
		return err
	}
	receiptData, err := os.ReadFile(filepath.Join(local, "gateway-receipt.key"))
	if err != nil {
		return err
	}
	receipt, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(receiptData)))
	if err != nil || len(receipt) != 32 {
		return errors.New("invalid fixture receipt key")
	}
	server, err := gateway.NewServer(gateway.ServerConfig{Backend: backend, PublicKeys: gateway.StaticPublicKeyResolver{OrganizationID: cfg.OrgID, RunnerID: cfg.RunnerID, PublicKey: public}, BasePath: "/runner-gateway", ReceiptKey: receipt, RunnerTokenSalt: "dbos-isolated-no-deployment-jobs", FetchWait: 2 * time.Second})
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: "127.0.0.1:39580", Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	err = httpServer.ListenAndServeTLS(filepath.Join(local, "gateway.crt"), filepath.Join(local, "gateway.key"))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func ensureObjectBuckets(connection *nats.Conn) error {
	js, err := connection.JetStream()
	if err != nil {
		return err
	}
	for _, bucket := range []string{"PO_RUNNER_BUNDLES", "PO_RUNNER_LOGS"} {
		if _, err = js.ObjectStore(bucket); err == nil {
			continue
		}
		if _, err = js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: bucket, Storage: nats.FileStorage}); err != nil {
			return err
		}
	}
	return nil
}
