package runner

import "encoding/json"

type RunnerError struct {
	Action        string   `json:"action"`
	Summary       string   `json:"summary"`
	Detail        string   `json:"detail,omitempty"`
	EntityType    Category `json:"entity_type,omitempty"`
	EntityId      string   `json:"entity_id,omitempty"`
	EntityVersion string   `json:"entity_version,omitempty"`
	CodeHint      string   `json:"code_hint,omitempty"`
}

type Category string

const (
	CategoryProvider Category = "provider"
	CategoryModule   Category = "module"
	CategoryOutput   Category = "output"
)

func (e *RunnerError) Error() string {
	jsonData, _ := json.Marshal(e)
	return string(jsonData)
}

func (e *RunnerError) Code() string {
	return "TF_DIAGNOSTIC_ERROR"
}
