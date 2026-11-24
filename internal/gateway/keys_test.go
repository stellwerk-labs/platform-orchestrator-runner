package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		require.Equal(t, []byte("public-key"), key.PublicKey)
	}
	require.EqualValues(t, 1, requests.Load())
}

func TestControlPlanePublicKeyResolverRejectsNonAgentRunner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"runner_configuration":{"type":"unknown"}}`))
	}))
	t.Cleanup(server.Close)

	resolver, err := NewControlPlanePublicKeyResolver(server.URL, server.Client(), time.Minute)
	require.NoError(t, err)
	_, err = resolver.Resolve(context.Background(), "my-org", "my-runner")
	require.ErrorContains(t, err, "type is unsupported")
}

func TestResolverClassifiesAllLegacyTypesWithoutAdoptingBackfilledUUID(t *testing.T) {
	for _, kind := range []string{"kubernetes", "kubernetes-agent", "kubernetes-eks", "kubernetes-gke", "serverless-ecs"} {
		t.Run(kind, func(t *testing.T) {
			_, public := gatewayKeyPair(t)
			configuration := map[string]any{"type": kind}
			if kind == "kubernetes-agent" {
				configuration["key"] = string(public)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "runner_configuration": configuration})
			}))
			t.Cleanup(server.Close)
			resolver, err := NewControlPlanePublicKeyResolver(server.URL, server.Client(), time.Hour)
			require.NoError(t, err)
			value, err := resolver.Resolve(t.Context(), "org", "site")
			require.NoError(t, err)
			require.Empty(t, value.RunnerUUID)
			if kind == "kubernetes-agent" {
				require.Equal(t, public, value.PublicKey)
			} else {
				require.Empty(t, value.PublicKey)
			}
		})
	}
}

func TestResolverCachesTheCompleteNativeAuthorityAndDisabledKey(t *testing.T) {
	uuid := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	_, public := gatewayKeyPair(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"uuid": uuid, "runner_configuration": map[string]any{"type": "native-agent", "key": string(public), "native_execution": nil}})
	}))
	t.Cleanup(server.Close)
	resolver, err := NewControlPlanePublicKeyResolver(server.URL, server.Client(), time.Hour)
	require.NoError(t, err)
	first, err := resolver.Resolve(t.Context(), "org", "site")
	require.NoError(t, err)
	require.Equal(t, uuid, first.RunnerUUID)
	first.PublicKey[0] = 'X'
	again, err := resolver.Resolve(t.Context(), "org", "site")
	require.NoError(t, err)
	require.Equal(t, public, again.PublicKey)
	require.Equal(t, uuid, again.RunnerUUID)
	require.EqualValues(t, 1, requests.Load())
	_, err = resolver.Resolve(t.Context(), "org", "0-"+uuid)
	require.Error(t, err)
	require.EqualValues(t, 1, requests.Load(), "reserved domain must fail before HTTP lookup")
	for _, invalid := range []string{"", "00000000-0000-0000-0000-000000000000", strings.ToUpper(uuid)} {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"uuid": invalid, "runner_configuration": map[string]any{"type": "native-agent", "key": string(public)}})
		}))
		other, err := NewControlPlanePublicKeyResolver(bad.URL, bad.Client(), time.Hour)
		require.NoError(t, err)
		_, err = other.Resolve(t.Context(), "org", "site")
		require.Error(t, err)
		bad.Close()
	}
}

func TestStaticAuthorityRefusesReservedDomainAndInvalidNativeUUID(t *testing.T) {
	uuid := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	for _, resolver := range []StaticPublicKeyResolver{
		{OrganizationID: "org", RunnerID: "0-" + uuid, PublicKey: []byte("key")},
		{OrganizationID: "org", RunnerID: "site", PublicKey: []byte("key"), RunnerUUID: strings.ToUpper(uuid)},
	} {
		_, err := resolver.Resolve(t.Context(), "org", resolver.RunnerID)
		require.Error(t, err)
	}
	resolver := StaticPublicKeyResolver{OrganizationID: "org", RunnerID: uuid, PublicKey: []byte("key")}
	value, err := resolver.Resolve(t.Context(), "org", uuid)
	require.NoError(t, err)
	require.Empty(t, value.RunnerUUID, "UUID-shaped legacy slug is a separate domain")
}
