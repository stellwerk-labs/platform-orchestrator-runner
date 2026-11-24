package rolloutexec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSavedApplyRefusesForeignUUIDBeforeScratchOrEffect(t *testing.T) {
	native := "3cbdc8ce-60d5-4ccc-9900-c7b6ae874897"
	for _, test := range []struct{ name, source, child string }{
		{"foreign", native, "7d736b82-2c14-4b4d-811a-b19a85c62b47"}, {"legacy_child", native, ""}, {"legacy_source", "", native},
		{"malformed_equal", "bad", "bad"}, {"nil_equal", "00000000-0000-0000-0000-000000000000", "00000000-0000-0000-0000-000000000000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "absent")
			e := Executor{Directory: dir}
			source := Request{ID: "plan", OrgID: "org", RunnerID: "site", EnvironmentID: "env", Backend: "local", Tool: "opentofu", Configuration: "resource", Manifest: json.RawMessage(`{}`), RunnerUUID: test.source}
			child := source
			child.RunnerUUID = test.child
			require.Equal(t, ValidRunnerUUID(test.source), validRequest(source))
			require.Equal(t, ValidRunnerUUID(test.child), validRequest(child))
			out, err := e.ApplySavedFrom(t.Context(), child, source, nil, "", func() error { t.Fatal("foreign authority reached effect"); return nil })
			require.ErrorContains(t, err, "authority")
			require.False(t, out.Started)
			_, statErr := os.Stat(dir)
			require.True(t, os.IsNotExist(statErr))
		})
	}
}
