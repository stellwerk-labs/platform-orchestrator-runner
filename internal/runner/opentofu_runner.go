package runner

import "context"

const defaultOpenTofuBinary = "tofu"

// NewOpenTofuRunner wraps base in an OpenTofuRunner, setting the default binary if none is provided.
func newOpenTofuRunner(base *baseRunner) *OpenTofuRunner {
	if base.binaryPath == "" {
		base.binaryPath = defaultOpenTofuBinary
	}
	return &OpenTofuRunner{base}
}

// OpenTofuRunner is a RunnerInterface implementation for OpenTofu.
// It enables OpenTofu-specific flags that are not supported by HashiCorp Terraform:
//   - -consolidate-errors (init, plan, apply, destroy)
//   - -show-sensitive     (output)
type OpenTofuRunner struct {
	*baseRunner
}

func (r *OpenTofuRunner) Init(ctx context.Context) (string, error) {
	return r.init(ctx, "-consolidate-errors")
}

func (r *OpenTofuRunner) Plan(ctx context.Context) (string, map[string]interface{}, *IaCChanges, error) {
	return r.plan(ctx, "-consolidate-errors")
}

func (r *OpenTofuRunner) Apply(ctx context.Context) (string, *IaCChanges, error) {
	return r.apply(ctx, "-consolidate-errors")
}

func (r *OpenTofuRunner) Destroy(ctx context.Context) (string, *IaCChanges, error) {
	return r.destroy(ctx, "-consolidate-errors")
}

func (r *OpenTofuRunner) Output(ctx context.Context) (map[string]interface{}, error) {
	return r.output(ctx, "-show-sensitive")
}
