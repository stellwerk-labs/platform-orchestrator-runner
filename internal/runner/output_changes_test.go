package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOutputChange_Compact_delete(t *testing.T) {
	assert.Equal(t, nil, OutputChange{Actions: []string{"delete"}}.Compact())
}

func TestOutputChange_Compact_no_change_primitive(t *testing.T) {
	assert.Equal(t, "hello", OutputChange{Actions: []string{"no-op"}, After: "hello"}.Compact())
}

func TestOutputChange_Compact_no_change_primitive_unknown(t *testing.T) {
	assert.Equal(t, "(known after apply)", OutputChange{Actions: []string{"create"}, AfterUnknown: true}.Compact())
}

func TestOutputChange_Compact_complex_unknowns(t *testing.T) {
	assert.Equal(t, map[string]interface{}{
		"a": 42,
		"b": map[string]interface{}{"c": []interface{}{1, 2, "(known after apply)"}},
	}, OutputChange{
		Actions: []string{"update"},
		After: map[string]interface{}{
			"a": 42,
			"b": map[string]interface{}{"c": []interface{}{1, 2, nil}},
		},
		AfterUnknown: map[string]interface{}{
			"b": map[string]interface{}{
				"c": []interface{}{false, false, true},
			},
		},
	}.Compact())
}
