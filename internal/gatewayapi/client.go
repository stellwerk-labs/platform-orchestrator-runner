package gatewayapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
)

const maxBundleBytes int64 = 64 << 20

type ClientConfig struct {
	BaseURL        string
	OrganizationID string
	RunnerID       string
	PrivateKey     []byte
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	Timeout        time.Duration
}

type Client struct {
	baseURL        string
	organizationID string
	runnerID       string
	privateKey     []byte
	httpClient     *http.Client
	now            func() time.Time
}

func NewClient(config ClientConfig) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(config.BaseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("runner gateway URL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("runner gateway URL must use HTTP or HTTPS")
	}
	tlsConfig, err := gatewayTLSConfig(config)
	if err != nil {
		return nil, err
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 40 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &Client{
		baseURL: parsed.String(), organizationID: config.OrganizationID, runnerID: config.RunnerID,
		privateKey: append([]byte(nil), config.PrivateKey...),
		httpClient: &http.Client{Transport: transport, Timeout: timeout}, now: time.Now,
	}, nil
}

func gatewayTLSConfig(config ClientConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if config.CAFile != "" {
		contents, err := os.ReadFile(config.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read runner gateway CA: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(contents) {
			return nil, errors.New("runner gateway CA file does not contain a PEM certificate")
		}
		tlsConfig.RootCAs = pool
	}
	if config.ClientCertFile != "" || config.ClientKeyFile != "" {
		if config.ClientCertFile == "" || config.ClientKeyFile == "" {
			return nil, errors.New("both runner gateway client certificate and key files are required for mTLS")
		}
		certificate, err := tls.LoadX509KeyPair(config.ClientCertFile, config.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load runner gateway client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return tlsConfig, nil
}

func (c *Client) NextCommand(ctx context.Context) (CommandResponse, error) {
	target := c.runnerURL("/commands/next")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return CommandResponse{}, err
	}
	if err := c.authorizeAgent(request); err != nil {
		return CommandResponse{}, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return CommandResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNoContent {
		return CommandResponse{}, ErrNoCommand
	}
	if response.StatusCode != http.StatusOK {
		return CommandResponse{}, responseError("fetch runner command", response)
	}
	var command CommandResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&command); err != nil {
		return CommandResponse{}, fmt.Errorf("decode runner command: %w", err)
	}
	if len(command.Command) == 0 || command.Receipt == "" {
		return CommandResponse{}, errors.New("runner command response is incomplete")
	}
	return command, nil
}

func (c *Client) Acknowledge(ctx context.Context, commandID, receipt string) error {
	return c.commandAction(ctx, commandID, "ack", ReceiptRequest{Receipt: receipt})
}

func (c *Client) Retry(ctx context.Context, commandID, receipt string, delay time.Duration) error {
	seconds := int(delay.Round(time.Second) / time.Second)
	return c.commandAction(ctx, commandID, "retry", RetryRequest{Receipt: receipt, DelaySeconds: seconds})
}

func (c *Client) Reject(ctx context.Context, commandID, receipt, reason string) error {
	return c.commandAction(ctx, commandID, "reject", RejectRequest{Receipt: receipt, Reason: reason})
}

func (c *Client) commandAction(ctx context.Context, commandID, action string, body any) error {
	contents, err := json.Marshal(body)
	if err != nil {
		return err
	}
	target := c.runnerURL("/commands/" + url.PathEscape(commandID) + "/" + action)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(contents))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if err := c.authorizeAgent(request); err != nil {
		return err
	}
	return c.expectStatus(request, http.StatusNoContent, "persist runner command action")
}

func (c *Client) PublishEvent(ctx context.Context, event hmessaging.EventEnvelope) error {
	contents, err := json.Marshal(event)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.runnerURL("/events"), bytes.NewReader(contents))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if err := c.authorizeAgent(request); err != nil {
		return err
	}
	return c.expectStatus(request, http.StatusAccepted, "persist runner event")
}

func (c *Client) GetBundle(ctx context.Context, deploymentID, deploymentToken string) ([]byte, error) {
	target := c.runnerURL("/deployments/" + url.PathEscape(deploymentID) + "/bundle")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set(DeploymentTokenHeader, deploymentToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, responseError("fetch deployment bundle", response)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxBundleBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maxBundleBytes {
		return nil, errors.New("deployment bundle exceeds the client limit")
	}
	return contents, nil
}

func (c *Client) PostResults(ctx context.Context, deploymentID, deploymentToken string, results any) error {
	contents, err := json.Marshal(results)
	if err != nil {
		return err
	}
	target := c.runnerURL("/deployments/" + url.PathEscape(deploymentID) + "/results")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(contents))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(DeploymentTokenHeader, deploymentToken)
	return c.expectStatus(request, http.StatusAccepted, "persist deployment result")
}

func (c *Client) PutLogs(ctx context.Context, environmentID, deploymentID, deploymentToken string, content io.Reader) error {
	target := c.runnerURL("/environments/" + url.PathEscape(environmentID) + "/deployments/" + url.PathEscape(deploymentID) + "/logs")
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target, content)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set(DeploymentTokenHeader, deploymentToken)
	return c.expectStatus(request, http.StatusAccepted, "persist encrypted deployment logs")
}

func (c *Client) runnerURL(suffix string) string {
	return c.baseURL + "/v1/orgs/" + url.PathEscape(c.organizationID) + "/runners/" + url.PathEscape(c.runnerID) + suffix
}

func (c *Client) authorizeAgent(request *http.Request) error {
	if len(c.privateKey) == 0 {
		return errors.New("runner private key is required for agent requests")
	}
	token, err := SignAgentToken(c.privateKey, c.organizationID, c.runnerID, c.now())
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (c *Client) expectStatus(request *http.Request, expected int, operation string) error {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != expected {
		return responseError(operation, response)
	}
	return nil
}

func responseError(operation string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return fmt.Errorf("%s: gateway returned %s: %s", operation, response.Status, strings.TrimSpace(string(body)))
}
