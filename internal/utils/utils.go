package utils

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/justinrixx/retryhttp"
	"github.com/pkg/errors"
)

func WrapHttpClientWithRetries(c *http.Client) *http.Client {
	c.Transport = retryhttp.New(
		retryhttp.WithTransport(c.Transport),
		retryhttp.WithMaxRetries(5),
		retryhttp.WithDelayFn(retryhttp.DefaultDelayFn),
		retryhttp.WithShouldRetryFn(retryhttp.CustomizedShouldRetryFn(retryhttp.CustomizedShouldRetryFnOptions{
			IdempotentMethods: []string{http.MethodGet, http.MethodDelete, http.MethodHead, http.MethodPut, http.MethodPost},
			RetryableStatusCodes: []int{
				http.StatusInternalServerError,
				http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
			},
		})),
	)
	return c
}

func SendResultsToApi(ctx context.Context, apiClient *platformorchestratorapi.ClientWithResponses, orgId, deploymentId, token string, results platformorchestratorapi.DeploymentResultsUpdateBody) error {
	resp, err := apiClient.UpdateDeploymentResultsWithResponse(ctx, orgId, uuid.MustParse(deploymentId), &platformorchestratorapi.UpdateDeploymentResultsParams{XDeploymentToken: token}, results)
	if err != nil {
		return errors.Wrap(err, "failed to update deployment results")
	} else if resp.StatusCode() == http.StatusBadRequest {
		return errors.Errorf("request is invalid: %s", resp.JSON400.Message)
	} else if resp.StatusCode() == http.StatusNotFound {
		return errors.Errorf("deployment not found: %s", resp.JSON404.Message)
	} else if resp.StatusCode() == http.StatusConflict {
		return errors.Errorf("deployment result's can't be updated: %s", resp.JSON409.Message)
	} else if resp.StatusCode() != http.StatusNoContent {
		return errors.Errorf("unexpected status code %d when creating module rule: %s", resp.StatusCode(), string(resp.Body))
	} else {
		slog.InfoContext(ctx, "deployment results sent successfully to api")
		return nil
	}
}

func EncryptBytes(logs []byte, recipient age.Recipient) (string, error) {
	var encryptedData = &bytes.Buffer{}
	w, err := age.Encrypt(encryptedData, recipient)
	if err != nil {
		return "", errors.Wrap(err, "failed to encrypt outputs with public key")
	}
	if _, err := w.Write(logs); err != nil {
		return "", errors.Wrap(err, "failed to write to encrypted file")
	}
	if err := w.Close(); err != nil {
		return "", errors.Wrap(err, "failed to close encrypted file")
	} else {
		return base64.StdEncoding.EncodeToString(encryptedData.Bytes()), nil
	}
}
