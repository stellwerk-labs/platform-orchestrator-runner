//go:build integration

package runner

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestObservationHTTPBackendLocksCancellationAndNoStateWrites(t *testing.T) {
	binaries := os.Getenv("IAC_TEST_BINARIES")
	if binaries == "" {
		binaries = "tofu,terraform"
	}
	for _, binary := range strings.Split(binaries, ",") {
		t.Run(binary, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			var mu sync.Mutex
			var state []byte
			writes := 0
			denyLock := false
			blockLock := false
			locked := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				switch r.Method {
				case "GET":
					data := append([]byte(nil), state...)
					mu.Unlock()
					if len(data) == 0 {
						w.WriteHeader(http.StatusNotFound)
					} else {
						_, _ = w.Write(data)
					}
				case "POST":
					// The backend's POST is the real persistent-state write boundary.
					state, _ = io.ReadAll(r.Body)
					writes++
					mu.Unlock()
					w.WriteHeader(http.StatusOK)
				case "LOCK":
					deny, block := denyLock, blockLock
					mu.Unlock()
					if block {
						select {
						case locked <- struct{}{}:
						default:
						}
						select {
						case <-r.Context().Done():
						case <-ctx.Done():
						}
						return
					}
					if deny {
						w.WriteHeader(http.StatusLocked)
						_, _ = w.Write([]byte(`{"ID":"SECRET_LOCK_CANARY","Operation":"OperationTypeApply"}`))
						return
					}
					w.WriteHeader(http.StatusOK)
				case "UNLOCK":
					mu.Unlock()
					w.WriteHeader(http.StatusOK)
				default:
					mu.Unlock()
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer func() { cancel(); server.Close() }()
			dir := t.TempDir()
			path, err := exec.LookPath(binary)
			require.NoError(t, err)
			source := fmt.Sprintf(`terraform {
 backend "http" {
  address = %q
  lock_address = %q
  unlock_address = %q
  lock_method = "LOCK"
  unlock_method = "UNLOCK"
  retry_max = 0
 }
}
resource "terraform_data" "managed" { input = "SECRET_CANARY" }
`, server.URL, server.URL, server.URL)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(source), 0600))
			run := func(args ...string) {
				t.Helper()
				out, err := exec.CommandContext(ctx, path, append([]string{"-chdir=" + dir}, args...)...).CombinedOutput()
				require.NoError(t, err, "fixture setup: %s", out)
			}
			run("init", "-input=false", "-no-color")
			run("apply", "-auto-approve", "-input=false", "-no-color")
			mu.Lock()
			baseline := append([]byte(nil), state...)
			baselineWrites := writes
			mu.Unlock()
			tool := "terraform"
			if filepath.Base(binary) == "tofu" {
				tool = "opentofu"
			}
			runner := &baseRunner{folder: dir, binaryPath: path, tool: tool, observationOnly: true}
			result, err := runner.Observe(ctx)
			require.NoError(t, err)
			require.Empty(t, result.ProposedChanges)
			mu.Lock()
			denyLock = true
			mu.Unlock()
			result, err = runner.Observe(ctx)
			require.Error(t, err)
			require.Nil(t, result)
			require.NotContains(t, err.Error(), "SECRET_LOCK_CANARY")
			mu.Lock()
			denyLock = false
			blockLock = true
			mu.Unlock()
			child, stop := context.WithCancel(ctx)
			finished := make(chan error, 1)
			go func() { _, err := runner.Observe(child); finished <- err }()
			select {
			case <-locked:
			case <-time.After(15 * time.Second):
				stop()
				t.Fatal("real backend lock request did not start")
			}
			stop()
			select {
			case err := <-finished:
				require.Error(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("observation did not terminate after cancellation")
			}
			mu.Lock()
			require.Equal(t, baseline, state)
			require.Equal(t, baselineWrites, writes, "observation wrote remote state")
			mu.Unlock()
			plans, err := filepath.Glob(filepath.Join(dir, ".observation-*"))
			require.NoError(t, err)
			require.Empty(t, plans)
		})
	}
}
