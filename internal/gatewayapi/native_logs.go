package gatewayapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
)

// PutNativeLog transfers frozen raw age bytes with independently bounded
// metadata. A slow full artifact has its own budget, separate from receipts.
func (c *Client) PutNativeLog(ctx context.Context, d nativelogs.Descriptor, ciphertext []byte) error {
	if d.VerifyCiphertext(ciphertext) != nil || d.Authority.OrganizationID != c.organizationID || d.Authority.Target.RunnerUUID != c.runnerUUID || d.Authority.Target.RunnerID != c.runnerID {
		return nativelogs.ErrArtifact
	}
	metadata, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if _, err = nativelogs.ParseDescriptor(metadata); err != nil {
		return err
	}
	target := c.runnerURL("/native-log-artifacts/environments/" + url.PathEscape(d.Authority.EnvironmentID) + "/deployments/" + url.PathEscape(d.Authority.DeploymentID))
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(ciphertext))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set(nativelogs.DescriptorHeader, base64.RawURLEncoding.EncodeToString(metadata))
	if err = c.authorizeAgent(request); err != nil {
		return err
	}
	client := *c.httpClient
	client.Timeout = 4 * time.Minute
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted {
		return responseError("persist native log artifact", response)
	}
	return nil
}
