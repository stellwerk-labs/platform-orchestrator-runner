package runner

import "context"

const defaultTerraformBinary = "terraform"

// newTerraformRunner wraps base in a TerraformRunner, setting the default binary if none is provided.
func newTerraformRunner(base *baseRunner) *TerraformRunner {
	if base.binaryPath == "" {
		base.binaryPath = defaultTerraformBinary
	}
	return &TerraformRunner{base}
}

// TerraformRunner is a RunnerInterface implementation for HashiCorp Terraform.
// It uses only the flags supported by the official Terraform CLI.
type TerraformRunner struct {
	*baseRunner
}

func (r *TerraformRunner) Init(ctx context.Context) (string, error) {
	return r.init(ctx)
}

func (r *TerraformRunner) Plan(ctx context.Context) (string, map[string]interface{}, *IaCChanges, error) {
	return r.plan(ctx)
}

func (r *TerraformRunner) Apply(ctx context.Context) (string, *IaCChanges, error) {
	return r.apply(ctx)
}

func (r *TerraformRunner) Destroy(ctx context.Context) (string, *IaCChanges, error) {
	return r.destroy(ctx)
}

// Output fetches all outputs.
// Note: HashiCorp Terraform redacts sensitive values in JSON output because it
// does not support -show-sensitive. Use OpenTofu if you need plaintext sensitive values.
func (r *TerraformRunner) Output(ctx context.Context) (map[string]interface{}, error) {
	return r.output(ctx)
}
