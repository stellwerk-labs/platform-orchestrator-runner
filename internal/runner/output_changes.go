package runner

import (
	"maps"
	"slices"
)

type OutputChange struct {
	Actions        []string    `json:"actions"`
	After          interface{} `json:"after"`
	AfterUnknown   interface{} `json:"after_unknown"`
	AfterSensitive bool        `json:"after_sensitive"`
}

func (oc OutputChange) Compact() interface{} {
	if len(oc.Actions) > 0 {
		switch oc.Actions[0] {
		case "no-op":
			return oc.After
		case "delete":
			return nil
		default:
			return compactUnknowns(oc.After, oc.AfterUnknown)
		}
	}
	return "(known after apply)"
}

func compactUnknowns(after, afterUnknown interface{}) interface{} {
	if asMap, ok := after.(map[string]interface{}); ok {
		out := maps.Clone(asMap)
		if unknownMap, ok := afterUnknown.(map[string]interface{}); ok {
			for k, v := range unknownMap {
				out[k] = compactUnknowns(asMap[k], v)
			}
		}
		return out
	} else if asSlice, ok := after.([]interface{}); ok {
		out := slices.Clone(asSlice)
		if unknownSlice, ok := afterUnknown.([]interface{}); ok {
			for i, v := range unknownSlice {
				out[i] = compactUnknowns(asSlice[i], v)
			}
		}
		return out
	}
	if b, ok := afterUnknown.(bool); !ok || !b {
		return after
	}
	return "(known after apply)"
}
