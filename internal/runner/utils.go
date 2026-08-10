package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
)

func runCmd(ctx context.Context, command string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	if stdout, err := cmd.CombinedOutput(); err != nil {
		return string(stdout), errors.Wrapf(err, "%s", string(stdout))
	} else {
		return string(stdout), nil
	}
}

func unarchiveBundleToFolder(gzipBuf []byte, destDir string) error {
	gr, err := gzip.NewReader(bytes.NewReader(gzipBuf))
	if err != nil {
		return errors.Wrap(err, "failed to create gzip reader")
	}
	defer func() {
		_ = gr.Close()
	}()

	tr := tar.NewReader(gr)

	for {
		header, err := tr.Next()

		switch {
		case err == io.EOF:
			return nil
		case err != nil:
			return errors.Wrap(err, "failed to read tar header")
		case header == nil:
			continue
		}

		name := filepath.Clean(header.Name)
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return errors.Errorf("bundle entry escapes destination: %q", header.Name)
		}
		target := filepath.Join(destDir, name)
		relative, err := filepath.Rel(destDir, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.Errorf("bundle entry escapes destination: %q", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return errors.Wrapf(err, "failed to create directory: %s", target)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return errors.Wrapf(err, "failed to create parent directories for: %s", target)
			}

			outFile, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return errors.Wrapf(err, "failed to open output file: %s", target)
			}
			_, copyErr := io.Copy(outFile, tr)
			closeErr := outFile.Close()
			if copyErr != nil {
				return errors.Wrapf(copyErr, "failed to write content to: %s", target)
			}
			if closeErr != nil {
				return errors.Wrapf(closeErr, "failed to close output file: %s", target)
			}
		}
	}
}
