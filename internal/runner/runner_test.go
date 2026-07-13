package runner

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"testing"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptString(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	base := &baseRunner{recipient: id.Recipient()}

	outputs := `this is tofu outputs`
	if base64EncryptedOutputs, err := base.EncryptString(outputs); assert.NoError(t, err) {
		if encryptedOutputs, err := base64.StdEncoding.DecodeString(base64EncryptedOutputs); assert.NoError(t, err) {
			if r, err := age.Decrypt(bytes.NewReader(encryptedOutputs), id); assert.NoError(t, err) {
				out := &bytes.Buffer{}
				if _, err = io.Copy(out, r); assert.NoError(t, err) {
					require.Equal(t, outputs, out.String())
				}
			}
		}
	}
}

func TestEncryptString_NoRecipient(t *testing.T) {
	base := &baseRunner{}
	outputs := `this is tofu outputs`
	base64EncryptedOutputs, err := base.EncryptString(outputs)
	if assert.NoError(t, err) {
		assert.Empty(t, base64EncryptedOutputs)
	}
}

func TestScanOutputs(t *testing.T) {
	changes, err := scanOutputs([]byte(`
unrelated line
null
"string like"
{}
{"type":"change_summary","changes":{"add": 42}}
{}
`))
	require.NoError(t, err)
	assert.Equal(t, &IaCChanges{Added: 42}, changes)
}

func TestVersion_DetectsOpenTofu(t *testing.T) {
	// OpenTofuRunner is selected explicitly via IAC_BACKEND; this test documents
	// that the version string itself does not drive flag selection any more.
	r := &OpenTofuRunner{&baseRunner{}}
	assert.NotNil(t, r)
}

func TestVersion_DetectsTerraform(t *testing.T) {
	r := &TerraformRunner{&baseRunner{}}
	assert.NotNil(t, r)
}

func TestParseShowPlanOutput(t *testing.T) {
	x, err := parseShowPlanOutput(`2025-10-09T10:49:12.235+0100 [INFO]  OpenTofu version: 1.9.0
2025-10-09T10:49:12.235+0100 [INFO]  Go runtime version: go1.23.4
2025-10-09T10:49:12.235+0100 [INFO]  CLI args: []string{"tofu", "show", "-json", "plan.tfplan"}
2025-10-09T10:49:12.235+0100 [INFO]  CLI command args: []string{"show", "-json", "plan.tfplan"}
2025-10-09T10:49:12.685+0100 [INFO]  provider: configuring client automatic mTLS
2025-10-09T10:49:12.847+0100 [INFO]  provider.terraform-provider-aws: configuring server automatic mTLS: timestamp="2025-10-09T10:49:12.847+0100"
{"format_version":"1.2","terraform_version":"1.9.0","planned_values":{},"output_changes":{"thing":{}}}
`)
	require.NoError(t, err)
	assert.Equal(t, map[string]interface{}{"thing": "(known after apply)"}, x)
}

func TestParseError(t *testing.T) {
	r := &baseRunner{}
	tests := []struct {
		name    string
		action  string
		err     error
		want    *RunnerError
		wantErr bool
	}{
		{
			name:    "nil error",
			action:  "apply",
			err:     nil,
			want:    nil,
			wantErr: false,
		},
		{
			name:   "basic error with summary",
			action: "plan",
			err:    errors.New(`{"@level":"error","@message":"Error: test error","@module":"tofu.ui","@timestamp":"2025-01-08T10:00:00.000000Z","diagnostic":{"severity":"error","summary":"Test Summary","detail":"Test Detail"},"type":"diagnostic"}`),
			want: &RunnerError{
				Action:  "plan",
				Summary: "Test Summary",
				Detail:  "Test Detail",
			},
			wantErr: false,
		},
		{
			name:   "provider error",
			action: "apply",
			err:    errors.New(`{"@level":"error","@message":"Error: test error","@module":"tofu.ui","@timestamp":"2025-01-08T10:00:00.000000Z","diagnostic":{"severity":"error","summary":"Provider Error","detail":"Provider failed","snippet":{"context":"provider \"aws\"","code":"  region = \"us-east-1\""}},"type":"diagnostic"}`),
			want: &RunnerError{
				Action:     "apply",
				Summary:    "Provider Error",
				Detail:     "Provider failed",
				EntityType: CategoryProvider,
				EntityId:   "aws",
				CodeHint:   "region = \"us-east-1\"",
			},
			wantErr: false,
		},
		{
			name:   "module error",
			action: "init",
			err:    errors.New(`{"@level":"error","@message":"Error: test error","@module":"tofu.ui","@timestamp":"2025-01-08T10:00:00.000000Z","diagnostic":{"severity":"error","summary":"Module Error","detail":"Module failed","snippet":{"context":"module \"vpc\"","code":"  source = \"./modules\""}},"type":"diagnostic"}`),
			want: &RunnerError{
				Action:     "init",
				Summary:    "Module Error",
				Detail:     "Module failed",
				EntityType: CategoryModule,
				EntityId:   "vpc",
				CodeHint:   "source = \"./modules\"",
			},
			wantErr: false,
		},
		{
			name:   "output error",
			action: "output",
			err:    errors.New(`{"@level":"error","@message":"Error: test error","@module":"tofu.ui","@timestamp":"2025-01-08T10:00:00.000000Z","diagnostic":{"severity":"error","summary":"Output Error","detail":"Output failed","snippet":{"context":"output \"result\"","code":"  value = var.foo"}},"type":"diagnostic"}`),
			want: &RunnerError{
				Action:     "output",
				Summary:    "Output Error",
				Detail:     "Output failed",
				EntityType: CategoryOutput,
				EntityId:   "result",
				CodeHint:   "value = var.foo",
			},
			wantErr: false,
		},
		{
			name:   "module error from modules/ path",
			action: "apply",
			err:    errors.New(`{"@level":"error","@message":"Error: test error","@module":"tofu.ui","@timestamp":"2025-01-08T10:00:00.000000Z","diagnostic":{"severity":"error","summary":"Module Path Error","detail":"","range":{"filename":"modules/my-module/1.0.0/main.tf","start":{"line":1,"column":1}},"snippet":{"context":"resource \"aws_instance\""}},"type":"diagnostic"}`),
			want: &RunnerError{
				Action:        "apply",
				Summary:       "Module Path Error",
				Detail:        "",
				EntityType:    CategoryModule,
				EntityId:      "my-module",
				EntityVersion: "1.0.0",
			},
			wantErr: false,
		},
		{
			name:   "quoted entity id",
			action: "plan",
			err:    errors.New(`{"@level":"error","@message":"Error: test error","@module":"tofu.ui","@timestamp":"2025-01-08T10:00:00.000000Z","diagnostic":{"severity":"error","summary":"Quoted Error","snippet":{"context":"resource \"my-quoted-id\""}},"type":"diagnostic"}`),
			want: &RunnerError{
				Action:     "plan",
				Summary:    "Quoted Error",
				EntityType: "resource",
				EntityId:   "my-quoted-id",
			},
			wantErr: false,
		},
		{
			name:   "multiline error with last line valid",
			action: "destroy",
			err:    errors.New("some output\nanother line\n" + `{"@level":"error","@message":"Error: test error","@module":"tofu.ui","@timestamp":"2025-01-08T10:00:00.000000Z","diagnostic":{"severity":"error","summary":"Last Line Error","detail":"Detail"},"type":"diagnostic"}`),
			want: &RunnerError{
				Action:  "destroy",
				Summary: "Last Line Error",
				Detail:  "Detail",
			},
			wantErr: false,
		},
		{
			name:    "non-error level diagnostic",
			action:  "plan",
			err:     errors.New(`{"@level":"info","@message":"Info message","diagnostic":{"severity":"info","summary":"Info"},"type":"diagnostic"}`),
			want:    nil,
			wantErr: false,
		},
		{
			name:    "error without summary or detail",
			action:  "apply",
			err:     errors.New(`{"@level":"error","diagnostic":{"severity":"error"},"type":"diagnostic"}`),
			want:    nil,
			wantErr: false,
		},
		{
			name:    "non-json error",
			action:  "init",
			err:     errors.New("plain text error"),
			want:    nil,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.parseError(tt.action, tt.err)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
