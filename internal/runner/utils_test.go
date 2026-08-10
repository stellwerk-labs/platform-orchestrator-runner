package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
