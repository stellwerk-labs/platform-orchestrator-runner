package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

const retainedProviderSchema = "stellwerk.retained-plan/v2"
const providerCopyLimit = 1 << 30

type retainedProviderFile struct {
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable"`
}

// Installed cache links are replaced by private copies before Plan can load them.
func (r *baseRunner) freezePlanProviders() error {
	if err := validateRetainedProviderConfig(r.tool); err != nil {
		return err
	}
	files, err := materializeProviders(r.folder)
	if err != nil {
		return errors.New("retained provider package unavailable")
	}
	r.retainedProviders = files
	return nil
}

func materializeProviders(folder string) (map[string]retainedProviderFile, error) {
	packages, err := filepath.Glob(filepath.Join(folder, ".terraform", "providers", "*", "*", "*", "*", runtime.GOOS+"_"+runtime.GOARCH))
	if err != nil || len(packages) > 64 {
		return nil, errors.New("provider package limit exceeded")
	}
	files := map[string]retainedProviderFile{}
	var size int64
	for _, path := range packages {
		if err = materializeProviderPackage(folder, path, files, &size); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func materializeProviderPackage(folder, path string, files map[string]retainedProviderFile, size *int64) error {
	if err := validateProviderParents(folder, path); err != nil {
		return err
	}
	source, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp(folder, ".retained-provider-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	prefix, err := filepath.Rel(folder, path)
	if err != nil {
		return err
	}
	err = copyProviderTree(source, staging, prefix, files, size)
	if err != nil {
		return err
	}
	if err = os.RemoveAll(path); err != nil {
		return err
	}
	return os.Rename(staging, path)
}

func copyProviderTree(source, destination, prefix string, files map[string]retainedProviderFile, size *int64) error {
	return filepath.WalkDir(source, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(destination, relative), 0700)
		}
		key := filepath.Join(prefix, relative)
		if len(files) >= 4096 {
			return errors.New("provider file limit exceeded")
		}
		file, err := copyProviderFile(current, filepath.Join(destination, relative), size)
		if err == nil {
			files[filepath.ToSlash(key)] = file
		}
		return err
	})
}

func validateProviderParents(folder, path string) error {
	for current := filepath.Dir(path); current != folder; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || filepath.Dir(current) == current {
			return errors.New("unsupported provider cache parent")
		}
	}
	return nil
}

func copyProviderFile(source, destination string, size *int64) (retainedProviderFile, error) {
	file, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return retainedProviderFile{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > providerCopyLimit-*size {
		return retainedProviderFile{}, errors.New("unsupported provider file")
	}
	mode := os.FileMode(0400) | info.Mode().Perm()&0100
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return retainedProviderFile{}, err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(file, info.Size()+1))
	closeErr := out.Close()
	if err != nil || n != info.Size() {
		return retainedProviderFile{}, errors.New("provider file changed")
	}
	*size += n
	return retainedProviderFile{SHA256: hex.EncodeToString(hash.Sum(nil)), Executable: mode&0100 != 0}, closeErr
}

func (r *baseRunner) restoreProviders(ctx context.Context, files map[string]retainedProviderFile) error {
	if err := r.validateNativeKubernetesEnvironment(); err != nil {
		return err
	}
	if err := validateRetainedProviderConfig(r.tool); err != nil {
		return err
	}
	lock := filepath.Join(r.folder, ".terraform.lock.hcl")
	info, err := os.Lstat(lock)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("retained provider lock unavailable")
	}
	_, err = runLocalCommand(ctx, r.binaryPath, "-chdir="+r.folder, "init", "-input=false", "-backend=false", "-get=false", "-lockfile=readonly", "-no-color")
	if err != nil {
		return errors.New("retained provider initialization failed")
	}
	actual, err := materializeProviders(r.folder)
	if err != nil || !maps.Equal(actual, files) {
		return errors.New("retained provider identity changed")
	}
	return nil
}
