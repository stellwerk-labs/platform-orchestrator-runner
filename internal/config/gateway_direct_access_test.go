package config

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestGatewayDirectAccessConfigurationProfiles(t *testing.T) {
	native := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	for _, test := range []struct {
		name, authority, runnerUUID string
		static, retained, valid     bool
	}{
		{"connected_legacy", "", "", false, true, true},
		{"connected_mixed", "http://dp:8080/base", "", false, true, true},
		{"static_legacy", "", "", true, false, true},
		{"static_native", "", native, true, false, true},
		{"static_authority", "http://dp:8080", "", true, false, false},
		{"static_native_authority", "http://dp:8080", native, true, false, false},
		{"static_native_retained", "", native, true, true, false},
		{"noncanonical_native", "", strings.ToUpper(native), true, false, false},
		{"zero_native", "", uuid.Nil.String(), true, false, false},
		{"relative_authority", "/dp", "", false, false, false},
		{"fragment_authority", "http://dp:8080#", "", false, false, false},
		{"double_slash_root", "http://dp:8080//", "", false, false, false},
		{"escaped_space", "http://dp:8080/%20", "", false, false, false},
		{"escaped_fragment", "http://dp:8080/%23", "", false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("NATS_URL", "not-a-live-broker")
			t.Setenv("RUNNER_GATEWAY_RECEIPT_KEY", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
			t.Setenv("RUNNER_TOKEN_SALT", uuid.NewString())
			t.Setenv("CONTROL_PLANE_URL", "http://core:8080")
			t.Setenv("RUNNER_GATEWAY_STATIC_ORG_ID", "")
			t.Setenv("RUNNER_GATEWAY_STATIC_RUNNER_ID", "")
			t.Setenv("RUNNER_GATEWAY_STATIC_PUBLIC_KEY_FILE", "")
			if test.static {
				t.Setenv("CONTROL_PLANE_URL", "")
				t.Setenv("RUNNER_GATEWAY_STATIC_ORG_ID", "O")
				t.Setenv("RUNNER_GATEWAY_STATIC_RUNNER_ID", "R")
				t.Setenv("RUNNER_GATEWAY_STATIC_PUBLIC_KEY_FILE", "operator-key.pem")
			}
			t.Setenv("RUNNER_GATEWAY_STATIC_RUNNER_UUID", test.runnerUUID)
			t.Setenv("RUNNER_GATEWAY_DEPLOYMENT_AUTHORITY_DP_URL", test.authority)
			t.Setenv("STELLWERK_RETAINED_PLANS_ENABLED", "false")
			t.Setenv("STELLWERK_RETAINED_PLAN_DP_URL", "")
			if test.retained {
				t.Setenv("STELLWERK_RETAINED_PLANS_ENABLED", "true")
				t.Setenv("STELLWERK_RETAINED_PLAN_DP_URL", "http://dp:8080")
			}
			conf, err := GetGatewayModeConfiguration()
			if !test.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.authority, conf.DeploymentAuthorityDPURL)
			require.Equal(t, test.runnerUUID, conf.StaticRunnerUUID)
		})
	}
}
