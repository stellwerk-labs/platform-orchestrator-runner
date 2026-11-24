//go:build integration

package rollout

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNativeObservationCancelRetainsAdmissionUntilRealProcessCompletes(t *testing.T) {
	for _, tool := range []string{"opentofu", "terraform"} {
		t.Run(tool, func(t *testing.T) {
			backend := &observationBackend{entered: make(chan struct{}, 1), release: make(chan struct{})}
			server := httptest.NewServer(http.HandlerFunc(backend.serve))
			defer server.Close()
			defer backend.unblock()
			f, trace := observationFixture(t, tool)
			main := fmt.Sprintf(`terraform {
 backend "http" {
  address = %q
  lock_address = %q
  unlock_address = %q
  lock_method = "LOCK"
  unlock_method = "UNLOCK"
  retry_max = 0
 }
}
resource "terraform_data" "managed" { input = "SECRET_CANCEL_CANARY" }
`, server.URL, server.URL, server.URL)
			prepareObservedState(t, tool, main)
			backend.mu.Lock()
			backend.block = true
			writes := backend.writes
			state := append([]byte(nil), backend.state...)
			backend.mu.Unlock()
			f.command.Request.Bundle = executionBundle(t, map[string]string{"main.tf": main})
			f.command.Request.BundleSHA256 = digest(f.command.Request.Bundle)
			_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
			require.NoError(t, err)
			select {
			case <-backend.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("real observation process did not reach backend lock")
			}
			before, err := f.engine.Get(t.Context(), f.command.ID)
			require.NoError(t, err)
			require.Equal(t, "observing", before.Phase)
			_, err = f.engine.Cancel(t.Context(), f.command.ID, "operator")
			requireCode(t, err, "observation_in_progress")
			after, err := f.engine.Get(t.Context(), f.command.ID)
			require.NoError(t, err)
			require.Equal(t, before, after, "cancel changed authoritative process ownership")
			_, _, ready, err := f.engine.Completion(t.Context(), after)
			require.NoError(t, err)
			require.False(t, ready, "active process manufactured terminal completion")
			next := f.command
			next.ID, next.Request.ID = "next-observation", "next-observation"
			_, err = f.engine.Submit(t.Context(), f.signed(t, next))
			requireCode(t, err, "environment_busy")
			backend.unblock()
			waitPhase(t, f.engine, f.command.ID, "completed")
			backend.mu.Lock()
			actualWrites := backend.writes
			actualState := append([]byte(nil), backend.state...)
			backend.mu.Unlock()
			require.Equal(t, writes, actualWrites, "observation wrote persistent state")
			require.Equal(t, state, actualState)
			require.Equal(t, 1, operationCount(t, trace, "apply"), "only fixture setup may apply")
		})
	}
}

type observationBackend struct {
	mu      sync.Mutex
	state   []byte
	writes  int
	block   bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *observationBackend) unblock() { b.once.Do(func() { close(b.release) }) }

func (b *observationBackend) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	switch r.Method {
	case http.MethodGet:
		data := append([]byte(nil), b.state...)
		b.mu.Unlock()
		if len(data) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	case http.MethodPost:
		b.state, _ = io.ReadAll(r.Body)
		b.writes++
		b.mu.Unlock()
	case "LOCK":
		block := b.block
		b.mu.Unlock()
		if block {
			select {
			case b.entered <- struct{}{}:
			default:
			}
			select {
			case <-b.release:
			case <-r.Context().Done():
			}
		}
	case "UNLOCK":
		b.mu.Unlock()
	default:
		b.mu.Unlock()
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
