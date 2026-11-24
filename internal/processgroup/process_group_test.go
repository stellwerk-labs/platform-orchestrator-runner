package processgroup

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunCleansDescendantsBeforeReapingNaturalExit(t *testing.T) {
	for _, test := range []struct {
		name, code, child string
	}{
		{"exit-0", "0", ""},
		{"exit-7", "7", ""},
		{"child-exit-0", "0", `(sleep 1; touch "$1") & `},
		{"child-exit-7", "7", `(sleep 1; touch "$1") & `},
	} {
		t.Run(test.name, func(t *testing.T) {
			late := filepath.Join(t.TempDir(), "late-child")
			cmd := exec.Command("/bin/sh", "-c", test.child+`printf stdout; printf stderr >&2; exit "$2"`, "sh", late, test.code)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			err := Run(t.Context(), cmd)
			require.Equal(t, "stdoutstderr", output.String())
			if test.code == "0" {
				require.NoError(t, err)
			} else {
				var exit *exec.ExitError
				require.True(t, errors.As(err, &exit))
				require.Equal(t, 7, exit.ExitCode())
			}
			if test.child != "" {
				time.Sleep(1200 * time.Millisecond)
			}
			require.NoFileExists(t, late, "a background child survived leader exit")
		})
	}
}
