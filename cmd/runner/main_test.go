package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/limitedlogsbuffer"
)

var limitedLogsBuffer = limitedlogsbuffer.NewLimitedLogsBuffer(&bytes.Buffer{}, maxLogsBufferSize)

func TestMainInner(t *testing.T) {
	t.Run("Standard Mode - runner only triggers standard mode", func(t *testing.T) {
		oldArgs := os.Args
		defer func() { os.Args = oldArgs }()

		os.Args = []string{"runner"}
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

		_, err := mainInner(context.Background(), limitedLogsBuffer)
		require.ErrorContains(t, err, "failed to read standard mode config")
	})

	t.Run("Standard Mode - runner with 'standard' triggers standard mode", func(t *testing.T) {
		oldArgs := os.Args
		defer func() { os.Args = oldArgs }()

		os.Args = []string{"runner", "standard"}
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

		_, err := mainInner(context.Background(), nil)
		require.ErrorContains(t, err, "failed to read standard mode config")
	})

	t.Run("Standard Mode - fails when gateway URL flag is set", func(t *testing.T) {
		oldArgs := os.Args
		defer func() { os.Args = oldArgs }()

		os.Args = []string{"runner", "--gateway-url=https://gateway.example.com/runner-gateway", "standard"}
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

		_, err := mainInner(context.Background(), limitedLogsBuffer)
		require.ErrorContains(t, err, "standard mode does not allow --gateway-url parameter")
	})

	t.Run("Remote Mode - runner with remote and gateway URL triggers remote mode", func(t *testing.T) {
		oldArgs := os.Args
		defer func() { os.Args = oldArgs }()

		os.Args = []string{"runner", "--gateway-url=https://gateway.example.com/runner-gateway", "remote"}
		t.Setenv("ORG_ID", "test-org")
		t.Setenv("RUNNER_ID", "remote-runner")
		t.Setenv("PRIVATE_KEY", "private-key")
		t.Setenv("RUNNER_GATEWAY_URL", "")
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

		_, err := mainInner(context.Background(), limitedLogsBuffer)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "RUNNER_GATEWAY_URL is required")
	})

	t.Run("Remote Mode - requires an endpoint when none is configured", func(t *testing.T) {
		oldArgs := os.Args
		defer func() { os.Args = oldArgs }()

		t.Setenv("ORG_ID", "test-org")
		t.Setenv("RUNNER_ID", "remote-runner")
		t.Setenv("PRIVATE_KEY", "private-key")
		os.Args = []string{"runner", "remote"}
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

		_, err := mainInner(context.Background(), limitedLogsBuffer)
		require.ErrorContains(t, err, "RUNNER_GATEWAY_URL is required")
	})

	t.Run("Invalid Mode - fails when unknown mode is provided", func(t *testing.T) {
		oldArgs := os.Args
		defer func() { os.Args = oldArgs }()

		os.Args = []string{"runner", "unknown-mode"}
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

		_, err := mainInner(context.Background(), limitedLogsBuffer)
		require.ErrorContains(t, err, "invalid mode: unknown-mode")
	})
}
