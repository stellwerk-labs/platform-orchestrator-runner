package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
)

func validControlEvent(event hmessaging.EventEnvelope) bool {
	if len(event.Payload) > signedcontrol.MaxBytes || !exactControlMembers(event.Payload, "version control_id rollout_id request_digest execution_digest outcome code phase revision") {
		return false
	}
	var result signedcontrol.Result
	if json.Unmarshal(event.Payload, &result) != nil {
		return false
	}
	return validControlResult(result) && result.RolloutID == event.DeploymentID &&
		event.CommandID == "pipeline-control:"+result.ControlID && event.EventID == event.CommandID+":result" && !event.CreatedAt.IsZero()

}

func validControlResult(result signedcontrol.Result) bool {
	return result.Version == signedcontrol.ResultVersion && signedcontrol.UUID(result.ControlID) &&
		signedcontrol.Hash(result.RequestDigest) && signedcontrol.Hash(result.ExecutionDigest) &&
		slices.Contains([]string{"applied", "refused"}, result.Outcome) && (result.Outcome == "refused") == (result.Code != "") &&
		len(result.Code) <= 128 && result.Phase != "" && len(result.Phase) <= 64 && result.Revision > 0
}

// Metadata-only control events cannot carry unknown or ambiguous body members.
func exactControlMembers(data []byte, names string) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	allowed := strings.Fields(names)
	seen := make(map[string]bool)
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] || !slices.Contains(allowed, name) {
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return false
		}
		seen[name] = true
	}
	_, err = decoder.Token()
	return err == nil && len(seen) == len(allowed) && decoder.Decode(new(any)) == io.EOF
}
