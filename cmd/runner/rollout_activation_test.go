package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRolloutActivationPrivateProjectedRecord(t *testing.T) {
	body, err := json.Marshal(rolloutActivation{Version: 1, State: "active"})
	require.NoError(t, err)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "..timestamp"), 0700))
	target := filepath.Join(root, "..timestamp", "activation.json")
	require.NoError(t, os.WriteFile(target, body, 0440))
	require.NoError(t, os.Symlink("..timestamp", filepath.Join(root, "..data")))
	path := filepath.Join(root, "activation.json")
	require.NoError(t, os.Symlink("..data/activation.json", path))
	actual, err := readActivationInput(path, 4096)
	require.NoError(t, err)
	require.Equal(t, body, actual)
	_, err = decodeRolloutActivation(actual)
	require.NoError(t, err)
	for _, invalid := range [][]byte{nil, []byte(`[]`), []byte(`{"version":1,"version":1}`), []byte(`{"version":1,"Version":1}`), []byte(`{"unknown":1}`), append(append([]byte{}, body...), []byte(` {}`)...)} {
		_, err = decodeRolloutActivation(invalid)
		require.ErrorIs(t, err, errRolloutActivation)
	}
	require.NoError(t, os.Chmod(target, 0644))
	_, err = readActivationInput(path, 4096)
	require.ErrorIs(t, err, errRolloutActivation)
	require.NoError(t, os.Chmod(target, 0600))
	for _, invalid := range [][]byte{nil, bytes.Repeat([]byte(" "), 4097)} {
		require.NoError(t, os.WriteFile(target, invalid, 0600))
		_, err = readActivationInput(path, 4096)
		require.ErrorIs(t, err, errRolloutActivation)
	}
	_, err = readActivationInput(root, 4096)
	require.ErrorIs(t, err, errRolloutActivation)
}
