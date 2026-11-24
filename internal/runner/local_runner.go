package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/processgroup"
)

type LocalOptions struct {
	NativeKubernetesProxy *string
	ObservationOnly       bool
	BackendConfigArgs     []string
	Directory             string
	Backend               config.IaCBackendType
	Identity              *age.X25519Identity
}

// LocalRunner uses the ordinary IaC implementation with site-local artifacts and keys.
type LocalRunner interface {
	RunnerInterface
	StageBundle([]byte) error
	ApplyDirect(context.Context) (string, *IaCChanges, error)
	PlanReview(context.Context) ([]byte, error)
	PlanText(context.Context) (string, error)
	RetainPlan(context.Context, RetainedBinding) (string, error)
	RestoreRetainedBytes(context.Context, RetainedBinding, []byte, string) error
}

func NewLocalRunner(options LocalOptions) (LocalRunner, error) {
	if !filepath.IsAbs(options.Directory) || options.Identity == nil {
		return nil, errors.New("invalid local runner configuration")
	}
	info, err := os.Stat(options.Directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("local runner requires a private directory")
	}
	base := &baseRunner{
		localInitArgs:   append([]string{}, options.BackendConfigArgs...),
		localProviders:  true,
		observationOnly: options.ObservationOnly,
		folder:          options.Directory, tool: string(options.Backend), recipient: options.Identity.Recipient(),
		keyLoader: func(context.Context) (string, error) { return options.Identity.String(), nil },
		command:   runLocalCommand,
	}
	if options.NativeKubernetesProxy != nil {
		proxy := *options.NativeKubernetesProxy
		base.nativeKubernetesProxy = &proxy
	}
	return newIaCRunner(base, options.Backend)
}

func newIaCRunner(base *baseRunner, backend config.IaCBackendType) (LocalRunner, error) {
	switch backend {
	case config.BackendTerraform:
		return newTerraformRunner(base), nil
	case config.BackendOpenTofu:
		return newOpenTofuRunner(base), nil
	default:
		return nil, errors.New("unsupported IaC backend")
	}
}

// RestoreRetainedBytes stages the immutable ciphertext in a fresh work directory.
func (r *baseRunner) RestoreRetainedBytes(ctx context.Context, binding RetainedBinding, ciphertext []byte, expected string) error {
	entries, err := os.ReadDir(r.folder)
	if err != nil || len(entries) != 0 || !retainedCiphertextMatches(ciphertext, expected) {
		return errors.New("invalid retained material")
	}
	if err := os.WriteFile(filepath.Join(r.folder, retainedFile), ciphertext, 0600); err != nil {
		return errors.New("retained material unavailable")
	}
	return r.RestoreRetainedPlan(ctx, binding, expected)
}

func runLocalCommand(ctx context.Context, program string, args ...string) (string, error) {
	cmd := exec.Command(program, args...)
	for _, variable := range infrastructureEnvironment() {
		name, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(name, "TF_CLI_ARGS") || strings.HasPrefix(name, "TF_LOG") || strings.HasPrefix(name, "TOFU_LOG") || name == "TF_DATA_DIR" || name == "TF_WORKSPACE" {
			continue
		}
		cmd.Env = append(cmd.Env, variable)
	}
	cmd.WaitDelay = time.Second
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := processgroup.Run(ctx, cmd)
	return output.String(), err
}

func (r *baseRunner) StageBundle(bundle []byte) error {
	if len(bundle) > 512<<10 {
		return errors.New("execution bundle exceeds limit")
	}
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	count := 0
	var total int64
	for hdr, readErr := tr.Next(); readErr != io.EOF; hdr, readErr = tr.Next() {
		if readErr != nil {
			return readErr
		}
		if hdr.Typeflag == tar.TypeDir {
			count++
		}
		if count > 10000 {
			return errors.New("execution bundle has too many entries")
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if err := unpackRetainedEntry(r.folder, tr, hdr, seen, &count, &total); err != nil {
			return err
		}
	}
	if !seen["main.tf"] {
		return errors.New("execution bundle has no main.tf")
	}
	return nil
}

func (r *baseRunner) ApplyDirect(ctx context.Context) (string, *IaCChanges, error) {
	if r.observationOnly {
		return "", nil, errors.New("observation cannot apply")
	}
	args := []string{"-chdir=" + r.folder, "apply", "-auto-approve", "-no-color", "-json", "-compact-warnings"}
	if r.tool == "opentofu" {
		args = append(args, "-consolidate-errors")
	}
	out, err := r.runCommand(ctx, args...)
	if err != nil {
		return out, nil, err
	}
	changes, err := scanOutputs([]byte(out))
	return out, changes, err
}
