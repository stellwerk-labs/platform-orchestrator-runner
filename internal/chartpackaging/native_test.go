//go:build chartintegration

package chartpackaging

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func render(t *testing.T, values ...string) ([]json.RawMessage, error) {
	t.Helper()
	chart, err := filepath.Abs("../../charts/platform-orchestrator-kubernetes-agent-runner")
	require.NoError(t, err)
	args := []string{"template", "runner", chart}
	for _, value := range values {
		args = append(args, "--set", value)
	}
	command := exec.CommandContext(t.Context(), "helm", args...)
	data, err := command.CombinedOutput()
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
	var objects []json.RawMessage
	for {
		var object json.RawMessage
		err = decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if len(object) > 0 && string(object) != "null" {
			objects = append(objects, object)
		}
	}
	return objects, nil
}

func nativeValues() []string {
	return []string{"executionMode=native", "native.configExistingSecret=runtime-config", "native.privateFilesExistingSecret=runtime-private"}
}

func TestNativeWorkersUseStablePrivateSlots(t *testing.T) {
	objects, err := render(t, nativeValues()...)
	require.NoError(t, err)
	var worker appsv1.StatefulSet
	var service corev1.Service
	for _, raw := range objects {
		var kind struct{ Kind string }
		require.NoError(t, json.Unmarshal(raw, &kind))
		switch kind.Kind {
		case "StatefulSet":
			require.NoError(t, json.Unmarshal(raw, &worker))
		case "Service":
			require.NoError(t, json.Unmarshal(raw, &service))
		case "Deployment", "Role", "RoleBinding", "Job", "ConfigMap":
			t.Fatalf("unexpected legacy/runtime authority resource: %s", kind.Kind)
		}
	}
	require.Equal(t, appsv1.OnDeleteStatefulSetStrategyType, worker.Spec.UpdateStrategy.Type)
	require.EqualValues(t, 2, *worker.Spec.Replicas)
	require.Equal(t, worker.Name, worker.Spec.ServiceName)
	require.Equal(t, "None", service.Spec.ClusterIP)
	require.Empty(t, service.Spec.Ports, "private operator API must not be exposed")
	require.Equal(t, worker.Spec.Selector.MatchLabels, service.Spec.Selector)
	pod := worker.Spec.Template.Spec
	require.False(t, *pod.AutomountServiceAccountToken)
	require.GreaterOrEqual(t, *pod.TerminationGracePeriodSeconds, int64(86520))
	var limits rollout.ExecutionLimits
	require.NoError(t, json.Unmarshal([]byte(`{"plan_seconds":1,"apply_seconds":1,"task_seconds":1}`), &limits))
	unsafeLimits, err := json.Marshal(rollout.ExecutionLimits{PlanSeconds: 1, ApplySeconds: *pod.TerminationGracePeriodSeconds - 120 + 1, TaskSeconds: 1})
	require.NoError(t, err)
	require.Error(t, json.Unmarshal(unsafeLimits, &limits), "accepted Apply ceilings must fit the rendered grace with 120 seconds for shutdown")
	require.Equal(t, int64(1000), *pod.SecurityContext.RunAsUser)
	require.Len(t, pod.Containers, 1)
	container := pod.Containers[0]
	require.Equal(t, []string{"/opt/runner/runner"}, container.Command)
	require.Equal(t, []string{"rollout"}, container.Args)
	require.Nil(t, container.LivenessProbe)
	require.Nil(t, container.Lifecycle)
	require.Equal(t, []string{"/opt/runner/runner", "--timeout=3s", "rollout-ready"}, container.ReadinessProbe.Exec.Command)
	require.Greater(t, container.ReadinessProbe.TimeoutSeconds, int32(3))
	require.Len(t, container.Env, 3)
	require.Empty(t, container.EnvFrom)
	require.Equal(t, "ROLLOUT_CONFIG_FILE", container.Env[0].Name)
	require.Equal(t, "/run/stellwerk/native/config/rollout.json", container.Env[0].Value)
	require.Equal(t, "ROLLOUT_WORKER_ID", container.Env[1].Name)
	require.Equal(t, "metadata.name", container.Env[1].ValueFrom.FieldRef.FieldPath)
	require.Equal(t, "HOME", container.Env[2].Name)
	require.Equal(t, "/work/home", container.Env[2].Value)
	require.Len(t, pod.InitContainers, 1)
	require.Len(t, pod.InitContainers[0].VolumeMounts, 1)
	require.Equal(t, container.Image, pod.InitContainers[0].Image)
	require.Equal(t, "work", pod.InitContainers[0].VolumeMounts[0].Name)
	for _, volume := range pod.Volumes {
		switch volume.Name {
		case "config":
			require.Equal(t, "runtime-config", volume.Secret.SecretName)
			require.EqualValues(t, 0440, *volume.Secret.DefaultMode)
		case "private":
			require.Equal(t, "runtime-private", volume.Secret.SecretName)
			require.EqualValues(t, 0440, *volume.Secret.DefaultMode)
		case "work", "tmp":
			require.NotNil(t, volume.EmptyDir)
		default:
			t.Fatalf("unexpected volume: %s", volume.Name)
		}
	}
	for _, mount := range container.VolumeMounts {
		require.Empty(t, mount.SubPath)
		if mount.Name == "config" || mount.Name == "private" {
			require.True(t, mount.ReadOnly)
		}
	}
}

func TestNativeInitializationHasSeparateAuthority(t *testing.T) {
	values := append(nativeValues(), "native.replicas=0", "native.initialization.enabled=true", "native.initialization.configExistingSecret=init-config", "native.initialization.privateFilesExistingSecret=init-private")
	objects, err := render(t, values...)
	require.NoError(t, err)
	var job batchv1.Job
	var worker appsv1.StatefulSet
	for _, raw := range objects {
		var kind struct{ Kind string }
		require.NoError(t, json.Unmarshal(raw, &kind))
		switch kind.Kind {
		case "Job":
			require.NoError(t, json.Unmarshal(raw, &job))
		case "StatefulSet":
			require.NoError(t, json.Unmarshal(raw, &worker))
		}
	}
	require.NotEmpty(t, job.Name)
	require.Empty(t, job.Annotations["helm.sh/hook"])
	require.Zero(t, *job.Spec.BackoffLimit)
	require.Zero(t, *worker.Spec.Replicas)
	pod := job.Spec.Template.Spec
	require.Equal(t, corev1.RestartPolicyNever, pod.RestartPolicy)
	require.False(t, *pod.AutomountServiceAccountToken)
	require.Equal(t, []string{"rollout-storage-init"}, pod.Containers[0].Args)
	require.Len(t, pod.Containers[0].Env, 2)
	for _, volume := range pod.Volumes {
		if volume.Secret != nil {
			require.Contains(t, []string{"init-config", "init-private"}, volume.Secret.SecretName)
		}
	}
	for _, volume := range worker.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			require.NotContains(t, []string{"init-config", "init-private"}, volume.Secret.SecretName)
		}
	}
}

func TestNativeInvalidProfilesRefuse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{"missing_runtime_config", []string{"native.configExistingSecret="}},
		{"missing_runtime_files", []string{"native.privateFilesExistingSecret="}},
		{"legacy_outbox", []string{"outbox.enabled=true"}},
		{"short_grace", []string{"native.terminationGracePeriodSeconds=7200"}},
		{"initialization_live_workers", []string{"native.initialization.enabled=true"}},
		{"initialization_missing_authority", []string{"native.initialization.enabled=true", "native.replicas=0"}},
		{"initialization_reuses_runtime_config", []string{"native.initialization.enabled=true", "native.replicas=0", "native.initialization.configExistingSecret=runtime-config", "native.initialization.privateFilesExistingSecret=init-private"}},
		{"initialization_reuses_runtime_files", []string{"native.initialization.enabled=true", "native.replicas=0", "native.initialization.configExistingSecret=init-config", "native.initialization.privateFilesExistingSecret=runtime-private"}},
		{"native_airgap_missing_uuid", []string{"gateway.mode=airgap", "protected-nats.enabled=true", "protected-nats.container.env.NATS_AUTH_TOKEN.valueFrom.secretKeyRef.name=nats", "platformOrchestrator.orgId=test-org", "platformOrchestrator.runnerId=test-runner", "airgap.gatewaySecret=gateway", "airgap.nats.existingSecret=nats"}},
		{"malformed_uuid", []string{"airgap.runnerUuid=not-a-uuid"}},
		{"zero_uuid", []string{"airgap.runnerUuid=00000000-0000-0000-0000-000000000000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := render(t, append(nativeValues(), tc.values...)...)
			require.Error(t, err)
		})
	}
}

func TestNativeAirgapStaticUUIDOptIn(t *testing.T) {
	uuid := "3cbdc8ce-60d5-4ccc-9900-c7b6ae874897"
	objects, err := render(t, append(nativeValues(), "gateway.mode=airgap", "protected-nats.enabled=true", "protected-nats.container.env.NATS_AUTH_TOKEN.valueFrom.secretKeyRef.name=nats", "platformOrchestrator.orgId=test-org", "platformOrchestrator.runnerId=test-runner", "airgap.gatewaySecret=gateway", "airgap.nats.existingSecret=nats", "airgap.runnerUuid="+uuid)...)
	require.NoError(t, err)
	var gateway appsv1.Deployment
	for _, raw := range objects {
		var kind struct{ Kind string }
		require.NoError(t, json.Unmarshal(raw, &kind))
		if kind.Kind == "Deployment" {
			require.NoError(t, json.Unmarshal(raw, &gateway))
		}
	}
	require.NotEmpty(t, gateway.Name)
	found := false
	for _, env := range gateway.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "RUNNER_GATEWAY_STATIC_RUNNER_UUID" {
			require.Equal(t, uuid, env.Value)
			found = true
		}
		require.NotEqual(t, "RUNNER_GATEWAY_DEPLOYMENT_AUTHORITY_DP_URL", env.Name)
		require.NotEqual(t, "RUNNER_GATEWAY_RETAINED_KEY_FORWARDING", env.Name)
	}
	require.True(t, found)
}

func TestNativeLegacyRendersRemainIdentical(t *testing.T) {
	baseline := os.Getenv("NATIVE_CHART_BASELINE")
	require.NotEmpty(t, baseline, "qualification must supply the frozen parent chart")
	for _, mode := range []string{"default", "simple", "edge", "airgap"} {
		t.Run(mode, func(t *testing.T) {
			values := []string{"platformOrchestrator.orgId=test-org", "platformOrchestrator.runnerId=test-runner", "gateway.privateKeyExistingSecret=identity", "gateway.mode=" + mode, "gateway.url=https://example.test/runner-gateway"}
			if mode == "default" {
				values = nil
			}
			if mode == "edge" {
				values = append(values, "outbox.enabled=true")
			}
			if mode == "airgap" {
				values = append(values, "protected-nats.enabled=true", "protected-nats.container.env.NATS_AUTH_TOKEN.valueFrom.secretKeyRef.name=nats", "airgap.gatewaySecret=gateway", "airgap.nats.existingSecret=nats")
			}
			actual, err := render(t, values...)
			require.NoError(t, err)
			args := []string{"template", "runner", baseline}
			for _, value := range values {
				args = append(args, "--set", value)
			}
			data, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
			require.NoError(t, err, "%s", data)
			decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
			var expected []json.RawMessage
			for {
				var object json.RawMessage
				err = decoder.Decode(&object)
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				if len(object) > 0 && string(object) != "null" {
					expected = append(expected, object)
				}
			}
			require.Equal(t, expected, actual)
		})
	}
}
