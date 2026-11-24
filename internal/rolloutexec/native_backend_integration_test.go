//go:build integration

package rolloutexec

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// This is an explicit real-backend qualification gate, never a missing-env skip.
func TestNativeKubernetesBackendCrossWorkerSavedPlan(t *testing.T) {
	path := os.Getenv("NATIVE_BACKEND_ADMIN_CONFIG")
	require.NotEmpty(t, path, "explicit owned Kubernetes test-admin configuration is required")
	configuration, err := clientcmd.BuildConfigFromFlags("", path)
	require.NoError(t, err)
	client, err := kubernetes.NewForConfig(configuration)
	require.NoError(t, err)
	for _, tool := range []string{"opentofu", "terraform"} {
		t.Run(tool, func(t *testing.T) { nativeKubernetesSavedPlan(t, tool, configuration, client) })
	}
}

func nativeKubernetesSavedPlan(t *testing.T, tool string, admin *rest.Config, client kubernetes.Interface) {
	t.Helper()
	program := tool
	if tool == "opentofu" {
		program = "tofu"
	}
	_, err := exec.LookPath(program)
	require.NoError(t, err, "both real Terraform and OpenTofu are required")
	run := uuid.NewString()
	namespace := "native-backend-" + run[:12]
	_, err = client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "stellwerk-test", "stellwerk.test/run": run}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		current, getErr := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		require.NoError(t, getErr)
		require.Equal(t, run, current.Labels["stellwerk.test/run"], "never delete a namespace whose ownership changed")
		require.NoError(t, client.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{}))
	})
	_, err = client.RbacV1().Roles(namespace).Create(t.Context(), &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "state"}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	aToken := nativeBackendServiceAccount(t, client, namespace, "worker-a")
	bToken := nativeBackendServiceAccount(t, client, namespace, "worker-b")
	trace := nativeBackendTrace(t, program)
	execution, request := fixture(t)
	source := t.TempDir()
	execution.Directory = filepath.Join(source, "work")
	require.NoError(t, os.Mkdir(execution.Directory, 0700))
	execution.KubernetesBackendConfigPath = filepath.Join(source, "source-kubeconfig")
	workerConfig := &clientcmdapi.Config{CurrentContext: "backend",
		Contexts:  map[string]*clientcmdapi.Context{"backend": {Cluster: "backend", AuthInfo: "backend"}},
		Clusters:  map[string]*clientcmdapi.Cluster{"backend": {Server: admin.Host, CertificateAuthorityData: admin.CAData}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"backend": {Token: aToken}}}
	require.NoError(t, os.WriteFile(execution.KubernetesBackendConfigPath, nativeBackendBytes(t, workerConfig), 0600))
	require.NoError(t, execution.PrepareNativeBackend())
	require.NoError(t, execution.PrepareNativeCatalogue(execution.Identity))
	request.RunnerUUID, request.Backend, request.Tool = run, "kubernetes", tool
	request.Configuration, request.Mode, request.DeploymentToken = "", "plan_only", ""
	request.Bundle = canonicalTestBundle(t, fmt.Sprintf(`terraform {
backend "kubernetes" {
namespace = %q
secret_suffix = "owned"
}
}
resource "terraform_data" "marker" { input = "reviewed" }
output "platform_orchestrator_metadata" { value = { marker = { value = terraform_data.marker.output } } }
`, namespace))
	request.BundleSHA256 = bytesDigest(request.Bundle)
	plan, err := execution.Plan(t.Context(), request)
	require.NoError(t, err, "%s", plan.Logs)
	require.Equal(t, 1, plan.Changes.Added)
	originalCiphertext := bytes.Clone(plan.Ciphertext)
	require.NoError(t, os.RemoveAll(source), "original worker source directory must be unavailable")
	require.NoError(t, client.RbacV1().RoleBindings(namespace).Delete(t.Context(), "worker-a", metav1.DeleteOptions{}))
	require.NoError(t, client.CoreV1().ServiceAccounts(namespace).Delete(t.Context(), "worker-a", metav1.DeleteOptions{}))
	oldConfig := rest.CopyConfig(admin)
	oldConfig.CertData, oldConfig.KeyData = nil, nil
	oldConfig.BearerToken = aToken
	oldClient, err := kubernetes.NewForConfig(oldConfig)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := oldClient.CoreV1().Secrets(namespace).List(t.Context(), metav1.ListOptions{})
		return apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err)
	}, 5*time.Second, 50*time.Millisecond, "old plan credentials must actually lose backend access before Apply")
	worker := *execution
	worker.Directory = t.TempDir()
	require.NoError(t, os.Chmod(worker.Directory, 0700))
	worker.KubernetesBackendConfigPath = filepath.Join(t.TempDir(), "new-worker-config")
	workerConfig.AuthInfos["backend"].Token = bToken
	require.NoError(t, os.WriteFile(worker.KubernetesBackendConfigPath, nativeBackendBytes(t, workerConfig), 0600))
	require.NoError(t, worker.PrepareNativeBackend())
	require.NoError(t, worker.PrepareNativeCatalogue(worker.Identity))
	child := request
	child.ID, child.Mode = "saved-child", "deploy"
	foreign := worker
	foreignConfig := workerConfig.DeepCopy()
	foreignConfig.Clusters["backend"].Server = "https://different-cluster.invalid"
	foreign.nativeBackend, err = parseNativeBackend(nativeBackendBytes(t, foreignConfig))
	require.NoError(t, err)
	refused, err := foreign.ApplySavedFrom(t.Context(), child, request, plan.Ciphertext, bytesDigest(plan.Ciphertext), func() error {
		t.Fatal("foreign backend reached the effect callback")
		return nil
	})
	require.ErrorIs(t, err, errNativeBackend)
	require.False(t, refused.Started)
	effects := 0
	outcome, err := worker.ApplySavedFrom(t.Context(), child, request, plan.Ciphertext, bytesDigest(plan.Ciphertext), func() error { effects++; return nil })
	require.NoError(t, err, "%s", outcome.Logs)
	require.True(t, outcome.Applied)
	require.Equal(t, 1, effects)
	require.Equal(t, originalCiphertext, plan.Ciphertext, "retained ciphertext must remain byte-identical")
	commands, err := os.ReadFile(trace)
	require.NoError(t, err)
	lines := strings.Fields(string(commands))
	require.Len(t, lines, 4, "exactly one Plan and Apply must be traced: %s", commands)
	require.Equal(t, "plan", lines[0])
	require.Equal(t, "apply", lines[2])
	require.Equal(t, lines[1], lines[3], "the real Apply must consume the exact saved plan bytes")
	secret, err := client.CoreV1().Secrets(namespace).Get(t.Context(), "tfstate-default-owned", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, secret.Data["tfstate"], "the real Kubernetes state destination must contain state")
	require.Len(t, outcome.Result.Metadata, 1)
	require.Equal(t, "reviewed", outcome.Result.Metadata[0].Metadata["value"])
	observation := child
	observation.Mode = "observation"
	observed, err := worker.Observe(t.Context(), observation)
	require.NoError(t, err)
	require.NotNil(t, observed.ObservationResult)
	require.True(t, bool(observed.ObservationResult.Complete))
	afterObservation, err := client.CoreV1().Secrets(namespace).Get(t.Context(), secret.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, secret.ResourceVersion, afterObservation.ResourceVersion, "observation must not write state")
	direct := child
	directOutcome, err := worker.Execute(t.Context(), direct, "apply", func() error { effects++; return nil })
	require.NoError(t, err, "%s", directOutcome.Logs)
	require.True(t, directOutcome.Applied)
	direct.Mode = "destroy"
	destroyed, err := worker.Execute(t.Context(), direct, "destroy", func() error { effects++; return nil })
	require.NoError(t, err, "%s", destroyed.Logs)
	require.True(t, destroyed.Applied)
	require.Equal(t, 1, destroyed.Changes.Remove)
	require.Equal(t, 3, effects, "one saved Apply, direct Apply and Destroy")
}

func nativeBackendServiceAccount(t *testing.T, client kubernetes.Interface, namespace, name string) string {
	t.Helper()
	_, err := client.CoreV1().ServiceAccounts(namespace).Create(t.Context(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.RbacV1().RoleBindings(namespace).Create(t.Context(), &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "state"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: namespace, Name: name}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	token, err := client.CoreV1().ServiceAccounts(namespace).CreateToken(t.Context(), name, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	require.NoError(t, err)
	return token.Status.Token
}

func nativeBackendTrace(t *testing.T, program string) string {
	t.Helper()
	binary, err := exec.LookPath(program)
	require.NoError(t, err)
	root := t.TempDir()
	trace := filepath.Join(root, "trace")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := `#!/bin/sh
hash_plan() {
  if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d ' ' -f 1
}
if [ "$2" = apply ]; then
  if [ -f "${1#-chdir=}/plan.tfplan" ]; then printf 'apply %s\n' "$(hash_plan "${1#-chdir=}/plan.tfplan")" >> ` + quote(trace) + `;
  else printf 'direct_apply\n' >> ` + quote(trace) + `; fi
fi
` + quote(binary) + ` "$@"
result=$?
if [ "$2" = plan ] && [ "$result" = 0 ] && [ -f "${1#-chdir=}/plan.tfplan" ]; then printf 'plan %s\n' "$(hash_plan "${1#-chdir=}/plan.tfplan")" >> ` + quote(trace) + `; fi
exit "$result"
`
	require.NoError(t, os.WriteFile(filepath.Join(root, program), []byte(script), 0700))
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	return trace
}
