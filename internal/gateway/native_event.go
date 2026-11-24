package gateway

import (
	"encoding/json"
	"strconv"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
)

// ValidNativeEventIdentity shares the connected Gateway identity rules with
// the bound file importer. Callers must first validate raw JSON and site scope.
func ValidNativeEventIdentity(event hmessaging.EventEnvelope) bool {
	return validNativeEventIdentity(event)
}

// Bind before Publish: a rejected event must not consume another completion's
// stream-wide deduplication identity.
func validNativeEventIdentity(event hmessaging.EventEnvelope) bool {
	id, err := uuid.Parse(event.DeploymentID)
	if err != nil || id == uuid.Nil || id.String() != event.DeploymentID {
		return false
	}
	switch event.Type {
	case nativelogs.ReadyEventType:
		data, err := json.Marshal(event)
		_, _, parseErr := nativelogs.ParseReady(data)
		return err == nil && parseErr == nil
	case "rollout-control-result":
		return validControlEvent(event)
	case "deployment-result":
		return event.EventID == event.DeploymentID+":deployment-result"
	case "rollout-progress":
		var body struct {
			Progress struct {
				ID       string `json:"id"`
				Revision int64  `json:"revision"`
			} `json:"progress"`
		}
		return json.Unmarshal(event.Payload, &body) == nil && body.Progress.ID == event.DeploymentID && body.Progress.Revision > 0 && event.EventID == event.DeploymentID+":"+strconv.FormatInt(body.Progress.Revision, 10)
	}
	return false
}
