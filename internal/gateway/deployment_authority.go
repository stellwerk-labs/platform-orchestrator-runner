package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
)

type deploymentAccess struct {
	Legacy          bool   `json:"legacy"`
	OrganizationID  string `json:"organization_id"`
	DeploymentID    string `json:"deployment_id"`
	RunnerID        string `json:"runner_id"`
	EnvironmentUUID string `json:"environment_uuid"`
}

func (s *Server) legacyDeploymentAccess(ctx context.Context, org, runner, deployment, token string) (deploymentAccess, error) {
	invalid := errors.New("legacy deployment authority unavailable")
	id, err := uuid.Parse(deployment)
	if err != nil || id == uuid.Nil || id.String() != deployment {
		return deploymentAccess{}, invalid
	}
	target, err := url.JoinPath(s.deploymentAuthorityDPURL, "v1", "orgs", org, "runners", runner, "deployments", deployment, "legacy-access")
	if err != nil {
		return deploymentAccess{}, invalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return deploymentAccess{}, invalid
	}
	req.Header.Set(gatewayapi.DeploymentTokenHeader, token)
	response, err := s.deploymentAuthorityClient.Do(req)
	if err != nil {
		return deploymentAccess{}, invalid
	}
	defer func() { _ = response.Body.Close() }()
	contentType, _, contentErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || contentErr != nil || contentType != "application/json" {
		return deploymentAccess{}, invalid
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		return deploymentAccess{}, invalid
	}
	var access deploymentAccess
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&access); err != nil {
		return deploymentAccess{}, invalid
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return deploymentAccess{}, invalid
	}
	environment, err := uuid.Parse(access.EnvironmentUUID)
	if err != nil || environment == uuid.Nil || environment.String() != access.EnvironmentUUID || !access.Legacy || access.OrganizationID != org || access.DeploymentID != deployment || access.RunnerID != runner {
		return deploymentAccess{}, invalid
	}
	return access, nil
}
