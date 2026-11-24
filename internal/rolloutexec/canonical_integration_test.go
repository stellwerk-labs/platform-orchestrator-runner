//go:build integration

package rolloutexec

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalRollbackRestoresActualEnvironment(t *testing.T) {
	for _, tool := range []string{"opentofu", "terraform"} {
		t.Run(tool, func(t *testing.T) { canonicalRollback(t, tool) })
	}
}

func canonicalRollback(t *testing.T, tool string) {
	t.Helper()
	binary := tool
	if tool == "opentofu" {
		binary = "tofu"
	}
	if _, err := exec.LookPath(binary); err != nil {
		t.Skip("IaC binary is not installed")
	}
	executor, request := fixture(t)
	request.Tool, request.Configuration = tool, ""
	request.DeploymentToken = "owned-fixture-admission"
	state := filepath.Join(t.TempDir(), "state.tfstate")
	effects := 0
	for index, step := range []struct{ mode, value string }{{"deploy", "baseline"}, {"deploy", "updated"}, {"rollback", "baseline"}} {
		request.ID, request.Mode = fmt.Sprintf("deployment-%d", index), step.mode
		program := fmt.Sprintf("terraform {\n backend \"local\" {\n path = %q\n }\n}\nresource \"terraform_data\" \"marker\" { input = %q }\noutput \"platform_orchestrator_metadata\" { value = { marker = { value = terraform_data.marker.output } } }\n", state, step.value)
		request.Bundle = canonicalTestBundle(t, program)
		request.BundleSHA256 = bytesDigest(request.Bundle)
		outcome, err := executor.Execute(context.Background(), request, "apply", func() error { effects++; return nil })
		require.NoError(t, err, "%s: %s", step.mode, outcome.Logs)
		require.True(t, outcome.Applied)
		require.Len(t, outcome.Result.Metadata, 1)
		require.Equal(t, step.value, outcome.Result.Metadata[0].Metadata["value"])
		if index == 0 {
			require.Equal(t, 1, outcome.Changes.Added)
		} else {
			require.Equal(t, 1, outcome.Changes.Change)
		}
	}
	require.Equal(t, 3, effects)
}

func canonicalTestBundle(t *testing.T, program string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "main.tf", Mode: 0600, Size: int64(len(program))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte(program)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
