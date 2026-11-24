//go:build integration

package rolloutexec

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBoundSavedPlanCannotChangeChildUUID(t *testing.T) {
	e, source := fixture(t)
	require.NoError(t, e.PrepareNativeCatalogue(e.Identity))
	source.RunnerUUID = "3cbdc8ce-60d5-4ccc-9900-c7b6ae874897"
	source.DeploymentToken = ""
	state := filepath.Join(t.TempDir(), "state")
	source.Configuration = "terraform {\n backend \"local\" { path = " + strconv.Quote(state) + " }\n}\nresource \"terraform_data\" \"bound\" { input = \"saved\" }\n"
	plan, err := e.Plan(t.Context(), source)
	require.NoError(t, err)
	child := source
	child.ID = "child"
	child.RunnerUUID = "7d736b82-2c14-4b4d-811a-b19a85c62b47"
	out, err := e.ApplySavedFrom(t.Context(), child, source, plan.Ciphertext, bytesDigest(plan.Ciphertext), func() error { t.Fatal("foreign child reached Apply"); return nil })
	require.ErrorContains(t, err, "authority")
	require.False(t, out.Started)
	entries, err := os.ReadDir(e.Directory)
	require.NoError(t, err)
	require.Empty(t, entries, "foreign child created/extracted scratch")
	// A copied key and valid ciphertext do not authorize another source profile either.
	for _, otherUUID := range []string{child.RunnerUUID, ""} {
		foreign := source
		foreign.RunnerUUID = otherUUID
		out, err = e.ApplySavedFrom(t.Context(), foreign, foreign, plan.Ciphertext, bytesDigest(plan.Ciphertext), func() error { t.Fatal("foreign source reached Apply"); return nil })
		require.Error(t, err)
		require.False(t, out.Started)
	}
	_, err = os.Stat(state)
	require.True(t, os.IsNotExist(err))
	child.RunnerUUID = source.RunnerUUID
	out, err = e.ApplySavedFrom(t.Context(), child, source, plan.Ciphertext, bytesDigest(plan.Ciphertext), nil)
	require.NoError(t, err)
	require.True(t, out.Applied)
	body, err := os.ReadFile(state)
	require.NoError(t, err)
	require.Contains(t, string(body), "saved")
}
