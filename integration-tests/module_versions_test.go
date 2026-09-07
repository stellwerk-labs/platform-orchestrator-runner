package integrationtests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-runner/integration-tests/clients/platformorchestratorcp"
)

// Module Management is Core functionality. Runner fixtures publish an explicit
// managed version and promote it before resolving a deployment through a rule.
func createManagedModuleWithResponse(t *testing.T, client platformorchestratorcp.ClientWithResponsesInterface, orgID string, body platformorchestratorcp.ModuleCreateBody) (*platformorchestratorcp.CreateModuleResponse, error) {
	t.Helper()
	response, err := client.CreateModuleWithResponse(t.Context(), orgID, body, func(_ context.Context, request *http.Request) error {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return err
		}
		payload["semantic_version"] = "1.0.0"
		if body.ModuleSourceCode == nil {
			// Fixture claim only. Trusted artifact verification is deferred.
			payload["artifact_digest"] = "sha256:" + strings.Repeat("0", 64)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		request.Body = io.NopCloser(bytes.NewReader(encoded))
		request.ContentLength = int64(len(encoded))
		return nil
	})
	if err != nil || response.StatusCode() != http.StatusCreated {
		return response, err
	}
	var created struct {
		Version string `json:"version_id"`
	}
	if err := json.Unmarshal(response.Body, &created); err != nil {
		return response, err
	}
	// The legacy fixture client intentionally retains the pre-management contract;
	// use its request editor for the new, versioned lifecycle endpoint.
	transition, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgID,
		platformorchestratorcp.RuleCreateBody{ModuleId: body.Id}, func(_ context.Context, request *http.Request) error {
			request.URL.Path = fmt.Sprintf("/orgs/%s/modules/%s/versions/%s/actions/promote", url.PathEscape(orgID), url.PathEscape(body.Id), url.PathEscape(created.Version))
			payload := []byte(`{"expected_resource_version":1,"reason":"Establish Runner integration-test Default"}`)
			request.Body = io.NopCloser(bytes.NewReader(payload))
			request.ContentLength = int64(len(payload))
			request.Header.Set("Idempotency-Key", "runner-fixture-promote")
			return nil
		})
	if err != nil {
		return response, err
	}
	if transition.StatusCode() != http.StatusOK {
		return response, fmt.Errorf("promote Runner fixture Module returned %d: %s", transition.StatusCode(), transition.Body)
	}
	return response, nil
}
