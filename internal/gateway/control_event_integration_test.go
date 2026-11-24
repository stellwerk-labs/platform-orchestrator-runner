//go:build integration

package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func TestNativeGatewayControlResultRefusesBeforeConsumingBrokerIdentity(t *testing.T) {
	f := newNativeGatewayFixture(t)
	route, err := hmessaging.NativeRunnerToken(f.uuids[0])
	require.NoError(t, err)
	result := signedcontrol.Result{Version: signedcontrol.ResultVersion, ControlID: uuid.NewString(), RolloutID: uuid.NewString(), RequestDigest: strings.Repeat("a", 64), ExecutionDigest: strings.Repeat("b", 64), Outcome: "applied", Phase: "cancelled", Revision: 2}
	payload, err := json.Marshal(result)
	require.NoError(t, err)
	event := hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: "pipeline-control:" + result.ControlID + ":result", CommandID: "pipeline-control:" + result.ControlID, OrganizationID: f.org, RunnerID: route, DeploymentID: result.RolloutID, Type: "rollout-control-result", CreatedAt: time.Now().UTC(), Payload: payload}
	wire, err := json.Marshal(event)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerEventSubject(f.org, route, "rollout-control-result")
	require.NoError(t, err)
	for _, bad := range [][]byte{
		[]byte(strings.Replace(string(wire), result.ControlID, uuid.NewString(), 1)),
		[]byte(strings.Replace(string(wire), `"request_digest":`, `"private_body":"SECRET","request_digest":`, 1)),
		[]byte(strings.Replace(string(wire), `"type":`, `"TYPE":`, 1)),

		[]byte(strings.Replace(string(wire), `"code":`, `"code phase":`, 1)),
		[]byte(strings.Replace(string(wire), `"code":`, `"code\u0020phase":`, 1)),
	} {
		response := f.request(t, http.MethodPost, "native-a", "/events", bad, f.private[0], f.uuids[0], "", nil)
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
		require.NoError(t, response.Body.Close())
		_, err = f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
		require.True(t, errors.Is(err, nats.ErrMsgNotFound))
	}
	response := f.request(t, http.MethodPost, "native-a", "/events", wire, f.private[0], f.uuids[0], "", nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	require.NoError(t, response.Body.Close())
	stored, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	require.Equal(t, wire, stored.Data)
	expected, err := hmessaging.NativeMessageID(f.org, route, "events", "rollout-control-result", event.EventID)
	require.NoError(t, err)
	require.Equal(t, expected, stored.Header.Get(nats.MsgIdHdr))
	response = f.request(t, http.MethodPost, "native-a", "/events", wire, f.private[0], f.uuids[0], "", nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	require.NoError(t, response.Body.Close())
	repeated, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	require.Equal(t, stored.Sequence, repeated.Sequence)
}
