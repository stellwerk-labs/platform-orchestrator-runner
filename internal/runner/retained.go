package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"filippo.io/age"
	"github.com/google/uuid"
)

const NativeRetainedSchema = "stellwerk.retained-plan/v3"

const retainedFile = ".stellwerk-retained-plan-v1.age"
const retainedLimit = 2 << 20
const retainedExpandedLimit = 64 << 20

type RetainedBinding struct {
	Schema             string `json:"schema"`
	OrgID              string `json:"org_id"`
	EnvironmentID      string `json:"environment_id"`
	RunnerID           string `json:"runner_id"`
	RunnerUUID         string `json:"runner_uuid,omitempty"`
	SourceDeploymentID string `json:"source_deployment_id"`
	BaseDeploymentID   string `json:"base_deployment_id"`
	Backend            string `json:"backend"`
	Tool               string `json:"tool"`
}
type retainedSnapshot struct {
	ProviderFiles map[string]retainedProviderFile `json:"provider_files,omitempty"`
	Binding       RetainedBinding                 `json:"binding"`
	OS            string                          `json:"os"`
	Arch          string                          `json:"arch"`
	ToolSHA256    string                          `json:"tool_sha256"`
	Archive       []byte                          `json:"archive"`
	Review        []byte                          `json:"review"`
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (r *baseRunner) toolDigest() (string, error) {
	p, err := exec.LookPath(r.binaryPath)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return digest(b), nil
}

// RetainPlan returns an encrypted, bounded snapshot. Its plaintext never enters logs.
func (r *baseRunner) RetainPlan(ctx context.Context, binding RetainedBinding) (string, error) {
	if !validRetainedBinding(binding) {
		return "", errors.New("retained binding refused")
	}
	if r.recipient == nil {
		return "", errors.New("retained plan requires encryption")
	}
	if r.localProviders && r.retainedProviders == nil {
		return "", errors.New("retained plan provider identity unavailable")
	}
	toolHash, err := r.toolDigest()
	if err != nil {
		return "", err
	}
	review, err := r.PlanReview(ctx)
	if err != nil {
		return "", err
	}
	archive, err := packRetainedFiles(r.folder, r.localProviders)
	if err != nil {
		return "", err
	}
	if len(r.retainedProviders) > 0 && binding.Schema != NativeRetainedSchema {
		binding.Schema = retainedProviderSchema
	}
	snap := retainedSnapshot{ProviderFiles: r.retainedProviders, Binding: binding, OS: runtime.GOOS, Arch: runtime.GOARCH, ToolSHA256: toolHash, Archive: archive, Review: review}
	b, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	encrypted, err := r.EncryptString(string(b))
	if err != nil {
		return "", err
	}
	if len(encrypted) > 3<<20 {
		return "", errors.New("retained ciphertext exceeds limit")
	}
	return encrypted, nil
}

func (r *baseRunner) PlanReview(ctx context.Context) ([]byte, error) {
	if r.observationOnly {
		return nil, errors.New("observation requires the safe observation executor")
	}
	review, err := r.runCommand(ctx, "-chdir="+r.folder, "show", "-no-color", "-json", r.planFile())
	if err != nil {
		return nil, errors.New("retained plan review failed")
	}
	if len(review) > retainedLimit {
		return nil, errors.New("retained plan review exceeds limit")
	}
	return []byte(review), nil
}

// PlanText renders the exact saved plan, retaining the CLI's sensitive-value masking.
func (r *baseRunner) PlanText(ctx context.Context) (string, error) {
	if r.observationOnly {
		return "", errors.New("observation requires the safe observation executor")
	}
	args := []string{"-chdir=" + r.folder, "show", "-no-color", "-json=false"}
	if r.tool == "opentofu" {
		args = append(args, "-show-sensitive=false")
	}
	return r.runCommand(ctx, append(args, r.planFile())...)
}

func packRetainedFiles(folder string, skipProviders bool) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	var total int64
	count := 0
	walk := retainedFileWalker(folder, tw, &count, &total, skipProviders)
	err := filepath.WalkDir(folder, walk)
	if err != nil {
		return nil, err
	}
	if err = tw.Close(); err != nil {
		return nil, err
	}
	if err = gz.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > retainedLimit {
		return nil, errors.New("retained archive exceeds limit")
	}
	return buf.Bytes(), nil
}

func retainedFileWalker(folder string, tw *tar.Writer, count *int, total *int64, skipProviders bool) fs.WalkDirFunc {
	return func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(folder, p)
		if err != nil {
			return err
		}
		if excluded, err := excludeRetainedEntry(rel, d, skipProviders); excluded {
			return err
		}
		if d.IsDir() {
			return nil
		}
		return writeRetainedFile(tw, p, rel, d, count, total)
	}
}

func excludeRetainedEntry(relative string, entry os.DirEntry, skipProviders bool) (bool, error) {
	switch relative {
	case ".":
		return true, nil
	case ".ssh-agent":
		if entry.IsDir() {
			return true, filepath.SkipDir
		}
		return true, nil
	case ".terraform/providers":
		if !skipProviders {
			return false, nil
		}
		if !entry.IsDir() {
			return true, errors.New("unsupported provider cache root")
		}
		return true, filepath.SkipDir
	default:
		return false, nil
	}
}

func writeRetainedFile(tw *tar.Writer, p, rel string, d os.DirEntry, count *int, total *int64) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("unsupported retained file type")
	}
	*count++
	*total += info.Size()
	if *count > 10000 || *total > retainedExpandedLimit {
		return errors.New("retained files exceed limit")
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), Size: info.Size()}
	if err = tw.WriteHeader(h); err != nil {
		return err
	}
	n, err := io.Copy(tw, io.LimitReader(f, info.Size()+1))
	if err != nil {
		return err
	}
	if n != info.Size() {
		return errors.New("retained file changed")
	}
	return nil
}

// RestoreRetainedPlan verifies the independently bound ciphertext before extracting it.
func (r *baseRunner) RestoreRetainedPlan(ctx context.Context, binding RetainedBinding, expected string) error {
	entries, err := os.ReadDir(r.folder)
	if err != nil || len(entries) != 1 || entries[0].Name() != retainedFile {
		return errors.New("invalid retained bundle")
	}
	b, err := os.ReadFile(filepath.Join(r.folder, retainedFile))
	if err != nil || !retainedCiphertextMatches(b, expected) {
		return errors.New("retained ciphertext mismatch")
	}
	plain, err := r.decryptRetained(ctx, b)
	if err != nil {
		return err
	}
	var snap retainedSnapshot
	if json.Unmarshal(plain, &snap) != nil || !retainedSnapshotMatches(snap, binding) {
		return errors.New("retained binding mismatch")
	}
	h, err := r.toolDigest()
	if err != nil || h != snap.ToolSHA256 {
		return errors.New("retained tool mismatch")
	}
	if err := unpackRetainedFiles(r.folder, snap.Archive); err != nil {
		return err
	}
	if len(snap.ProviderFiles) > 0 {
		if err := r.restoreProviders(ctx, snap.ProviderFiles); err != nil {
			return err
		}
	}
	return os.Remove(filepath.Join(r.folder, retainedFile))
}

func retainedCiphertextMatches(b []byte, expected string) bool {
	return len(b) <= 3<<20 && digest(b) == expected
}

func retainedSnapshotMatches(snap retainedSnapshot, binding RetainedBinding) bool {
	if !validRetainedBinding(binding) || !validRetainedBinding(snap.Binding) {
		return false
	}
	if snap.Binding.Schema == retainedProviderSchema {
		if binding.Schema != "stellwerk.retained-plan/v1" || len(snap.ProviderFiles) == 0 {
			return false
		}
		snap.Binding.Schema = binding.Schema
	} else if snap.Binding.Schema != NativeRetainedSchema && len(snap.ProviderFiles) != 0 {
		return false
	}
	return snap.Binding == binding && snap.OS == runtime.GOOS && snap.Arch == runtime.GOARCH
}

func (r *baseRunner) decryptRetained(ctx context.Context, b []byte) ([]byte, error) {
	identity, err := r.keyLoader(ctx)
	if err != nil {
		return nil, errors.New("retained key unavailable")
	}
	id, err := age.ParseX25519Identity(identity)
	if err != nil {
		return nil, errors.New("invalid retained key")
	}
	decrypted, err := age.Decrypt(bytes.NewReader(b), id)
	if err != nil {
		return nil, errors.New("retained decryption failed")
	}
	plain, err := io.ReadAll(io.LimitReader(decrypted, 5<<20))
	if err != nil || len(plain) >= 5<<20 {
		return nil, errors.New("retained snapshot exceeds limit")
	}
	return plain, nil
}

func unpackRetainedFiles(folder string, archive []byte) error {
	if len(archive) > retainedLimit {
		return errors.New("retained archive exceeds limit")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return errors.New("invalid retained archive")
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	var total int64
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid retained archive")
		}
		if err := unpackRetainedEntry(folder, tr, hdr, seen, &count, &total); err != nil {
			return err
		}

	}
	if !seen["plan.tfplan"] || !seen["main.tf"] {
		return errors.New("retained material incomplete")
	}
	return nil
}

func unpackRetainedEntry(folder string, tr *tar.Reader, hdr *tar.Header, seen map[string]bool, count *int, total *int64) error {
	name := hdr.Name
	if !validRetainedEntry(hdr) || seen[name] {
		return errors.New("invalid retained entry")
	}
	seen[name] = true
	*count++
	*total += hdr.Size
	if *count > 10000 || *total > retainedExpandedLimit {
		return errors.New("retained files exceed limit")
	}
	target := filepath.Join(folder, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(hdr.Mode)&0700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, tr)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func validRetainedEntry(hdr *tar.Header) bool {
	name := hdr.Name
	return name != "" && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../") && name != retainedFile && hdr.Typeflag == tar.TypeReg && hdr.Size >= 0
}

func validRetainedBinding(binding RetainedBinding) bool {
	if binding.Schema == NativeRetainedSchema {
		id, err := uuid.Parse(binding.RunnerUUID)
		return err == nil && id != uuid.Nil && id.String() == binding.RunnerUUID
	}
	return (binding.Schema == "stellwerk.retained-plan/v1" || binding.Schema == retainedProviderSchema) && binding.RunnerUUID == ""
}
