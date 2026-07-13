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

	t.Run("Standard Mode - fails when 'remote' mode has URL", func(t *testing.T) {
		oldArgs := os.Args
		defer func() { os.Args = oldArgs }()

		os.Args = []string{"runner", "--remote-connect=https://another-valid-url.com", "standard"}
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

		_, err := mainInner(context.Background(), limitedLogsBuffer)
		require.ErrorContains(t, err, "standard mode does not allow --remote-connect parameter")
	})

	t.Run("Remote Mode - runner with 'remote' and valid --remote-connect triggers remote mode", func(t *testing.T) {
		oldArgs := os.Args
		defer func() { os.Args = oldArgs }()

		os.Args = []string{"runner", "--remote-connect=https://another-valid-url.com", "remote"}
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

		_, err := mainInner(context.Background(), limitedLogsBuffer)
		require.ErrorContains(t, err, "failed to read remote mode config")
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
