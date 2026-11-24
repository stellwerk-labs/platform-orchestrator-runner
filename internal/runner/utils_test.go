package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUnarchiveBundleRejectsEntriesOutsideDestination(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute"} {
		t.Run(name, func(t *testing.T) {
			var bundle bytes.Buffer
			gzipWriter := gzip.NewWriter(&bundle)
			tarWriter := tar.NewWriter(gzipWriter)
			require.NoError(t, tarWriter.WriteHeader(&tar.Header{
				Name: name, Mode: 0o600, Size: 1, ModTime: time.Now(), Typeflag: tar.TypeReg,
			}))
			_, err := tarWriter.Write([]byte("x"))
			require.NoError(t, err)
			require.NoError(t, tarWriter.Close())
			require.NoError(t, gzipWriter.Close())

			err = unarchiveBundleToFolder(bundle.Bytes(), t.TempDir())
			require.ErrorContains(t, err, "escapes destination")
		})
	}
}

func TestLegacyRunCmdPreservesCombinedOutputAndExitStatus(t *testing.T) {
	for _, test := range []struct {
		name, script string
		code         int
		cancel       bool
	}{
		{"success", "exit 0", 0, false},
		{"nonzero", "exit 7", 7, false},
		{"canceled", `trap '' INT; (sleep 1; touch "$1") & wait`, -1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			late := filepath.Join(t.TempDir(), "late-child")
			output, err := runCmd(ctx, "/bin/sh", "-c", "printf stdout; printf stderr >&2; "+test.script, "sh", late)
			require.Equal(t, "stdoutstderr", output)
			if test.code == 0 {
				require.NoError(t, err)
			} else {
				var exit *exec.ExitError
				require.True(t, errors.As(err, &exit))
				require.Equal(t, test.code, exit.ExitCode())
				require.Equal(t, output+": "+exit.Error(), err.Error())
			}
			if test.cancel {
				time.Sleep(800 * time.Millisecond)
				require.NoFileExists(t, late, "legacy command cancellation left a live child")
			}
		})
	}
}
