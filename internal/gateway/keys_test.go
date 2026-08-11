package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestControlPlanePublicKeyResolverUsesInternalRunnerShapeAndCache(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		require.Equal(t, "/internal/orgs/my-org/runners/my-runner", request.URL.Path)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"runner_configuration":{"type":"kubernetes-agent","key":"public-key","job":{"namespace":"jobs","service_account":"runner"}}}`))
	}))
	t.Cleanup(server.Close)

	resolver, err := NewControlPlanePublicKeyResolver(server.URL, server.Client(), time.Minute)
	require.NoError(t, err)
	for range 2 {
		key, resolveErr := resolver.Resolve(context.Background(), "my-org", "my-runner")
		require.NoError(t, resolveErr)
		require.Equal(t, []byte("public-key"), key)
	}
	require.EqualValues(t, 1, requests.Load())
}

func TestControlPlanePublicKeyResolverRejectsNonAgentRunner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"runner_configuration":{"type":"kubernetes"}}`))
	}))
	t.Cleanup(server.Close)

	resolver, err := NewControlPlanePublicKeyResolver(server.URL, server.Client(), time.Minute)
	require.NoError(t, err)
	_, err = resolver.Resolve(context.Background(), "my-org", "my-runner")
	require.ErrorContains(t, err, "not a Kubernetes agent")
}
