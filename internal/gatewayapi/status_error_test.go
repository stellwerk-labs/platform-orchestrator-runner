package gatewayapi

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayStatusErrorDoesNotRetainResponseBody(t *testing.T) {
	err := responseError("ack command", &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("PRIVATE_GATEWAY_BODY_TOKEN"))})
	var status *HTTPStatusError
	require.True(t, errors.As(err, &status))
	require.Equal(t, http.StatusForbidden, status.StatusCode)
	require.NotContains(t, err.Error(), "PRIVATE_GATEWAY_BODY_TOKEN")
}
