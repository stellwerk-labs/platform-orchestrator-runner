package rolloutexec

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func nativeBackendConfig(t *testing.T) *clientcmdapi.Config {
	t.Helper()
	ca, err := os.ReadFile("testdata/native-backend-ca.pem")
	require.NoError(t, err)
	return &clientcmdapi.Config{CurrentContext: "selected",
		Contexts:  map[string]*clientcmdapi.Context{"selected": {Cluster: "cluster", AuthInfo: "user"}},
		Clusters:  map[string]*clientcmdapi.Cluster{"cluster": {Server: "https://cluster.test/kubernetes", CertificateAuthorityData: ca}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"user": {Token: "PRIVATE_BACKEND_OLD_TOKEN"}}}
}

func nativeBackendBytes(t *testing.T, config *clientcmdapi.Config) []byte {
	t.Helper()
	data, err := clientcmd.Write(*config)
	require.NoError(t, err)
	return data
}

func TestNativeBackendIdentitySeparatesDestinationFromAuthentication(t *testing.T) {
	config := nativeBackendConfig(t)
	prepared, err := parseNativeBackend(nativeBackendBytes(t, config))
	require.NoError(t, err)
	require.NotContains(t, string(prepared.destination), "PRIVATE_BACKEND")
	for _, change := range []struct {
		name string
		run  func(*clientcmdapi.Config)
	}{
		{"token", func(c *clientcmdapi.Config) { c.AuthInfos["user"].Token = "PRIVATE_BACKEND_NEW_TOKEN" }},
		{"unrelated", func(c *clientcmdapi.Config) {
			c.Clusters["ignored"] = &clientcmdapi.Cluster{Server: "http://unselected", CertificateAuthority: "/absent/private/file"}
			c.AuthInfos["ignored"] = &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{Command: "/never/execute"}}
		}},
		{"aliases", func(c *clientcmdapi.Config) {
			context := c.Contexts["selected"]
			context.Cluster, context.AuthInfo = "renamed", "renamed"
			c.Clusters["renamed"], c.AuthInfos["renamed"] = c.Clusters["cluster"], c.AuthInfos["user"]
			c.Contexts["renamed"] = context
			delete(c.Contexts, "selected")
			c.CurrentContext = "renamed"
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := config.DeepCopy()
			change.run(changed)
			next, err := parseNativeBackend(append([]byte("# ignored comment\n"), nativeBackendBytes(t, changed)...))
			require.NoError(t, err)
			require.Equal(t, prepared.destination, next.destination)
		})
	}
	for _, change := range []struct {
		name string
		run  func(*clientcmdapi.Cluster)
	}{
		{"endpoint", func(c *clientcmdapi.Cluster) { c.Server = "https://another.test/kubernetes" }},
		{"trust bytes", func(c *clientcmdapi.Cluster) { c.CertificateAuthorityData = append(c.CertificateAuthorityData, '\n') }},
		{"TLS name", func(c *clientcmdapi.Cluster) { c.TLSServerName = "cluster.test" }},
		{"proxy", func(c *clientcmdapi.Cluster) { c.ProxyURL = "http://proxy.test:8080" }},
		{"compression", func(c *clientcmdapi.Cluster) { c.DisableCompression = true }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := config.DeepCopy()
			change.run(changed.Clusters["cluster"])
			next, err := parseNativeBackend(nativeBackendBytes(t, changed))
			require.NoError(t, err)
			require.NotEqual(t, prepared.destination, next.destination)
		})
	}
}

func TestNativeBackendRefusesImplicitAndDynamicInputs(t *testing.T) {
	for _, change := range []struct {
		name string
		run  func(*clientcmdapi.Config)
	}{
		{"no context", func(c *clientcmdapi.Config) { c.CurrentContext = "" }},
		{"missing context", func(c *clientcmdapi.Config) { c.CurrentContext = "absent" }},
		{"no CA", func(c *clientcmdapi.Config) { c.Clusters["cluster"].CertificateAuthorityData = nil }},
		{"bad CA", func(c *clientcmdapi.Config) {
			c.Clusters["cluster"].CertificateAuthorityData = []byte("PRIVATE_BACKEND_CANARY")
		}},
		{"external CA", func(c *clientcmdapi.Config) {
			c.Clusters["cluster"].CertificateAuthority = "/never/read/PRIVATE_BACKEND_CANARY"
		}},
		{"insecure", func(c *clientcmdapi.Config) { c.Clusters["cluster"].InsecureSkipTLSVerify = true }},
		{"external cert", func(c *clientcmdapi.Config) { c.AuthInfos["user"].ClientCertificate = "/never/read" }},
		{"external key", func(c *clientcmdapi.Config) { c.AuthInfos["user"].ClientKey = "/never/read" }},
		{"external token", func(c *clientcmdapi.Config) { c.AuthInfos["user"].TokenFile = "/never/read" }},
		{"exec", func(c *clientcmdapi.Config) {
			c.AuthInfos["user"].Exec = &clientcmdapi.ExecConfig{Command: "/never/execute"}
		}},
		{"auth provider", func(c *clientcmdapi.Config) {
			c.AuthInfos["user"].AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "gcp"}
		}},
		{"missing auth", func(c *clientcmdapi.Config) { c.Contexts["selected"].AuthInfo = "" }},
		{"global extension", func(c *clientcmdapi.Config) {
			c.Extensions = map[string]runtime.Object{"extension": &runtime.Unknown{Raw: []byte(`{"private":"canary"}`)}}
		}},
		{"preferences extension", func(c *clientcmdapi.Config) {
			c.Preferences.Extensions = map[string]runtime.Object{"extension": &runtime.Unknown{Raw: []byte(`{"private":"canary"}`)}}
		}},
		{"context extension", func(c *clientcmdapi.Config) {
			c.Contexts["selected"].Extensions = map[string]runtime.Object{"extension": &runtime.Unknown{Raw: []byte(`{"private":"canary"}`)}}
		}},
		{"cluster extension", func(c *clientcmdapi.Config) {
			c.Clusters["cluster"].Extensions = map[string]runtime.Object{"extension": &runtime.Unknown{Raw: []byte(`{"private":"canary"}`)}}
		}},
		{"auth extension", func(c *clientcmdapi.Config) {
			c.AuthInfos["user"].Extensions = map[string]runtime.Object{"extension": &runtime.Unknown{Raw: []byte(`{"private":"canary"}`)}}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			config := nativeBackendConfig(t)
			change.run(config)
			_, err := parseNativeBackend(nativeBackendBytes(t, config))
			require.ErrorIs(t, err, errNativeBackend)
			require.NotContains(t, err.Error(), "PRIVATE_BACKEND_CANARY")
		})
	}
	for _, raw := range [][]byte{nil, []byte("null"), []byte("{"), bytes.Repeat([]byte(" "), nativeBackendLimit+1)} {
		_, err := parseNativeBackend(raw)
		require.ErrorIs(t, err, errNativeBackend)
	}
}

func TestNativeBackendURLPolicy(t *testing.T) {
	for _, endpoint := range []string{"http://cluster.test", "https:///missing", "https://user:PRIVATE_BACKEND_CANARY@host", "https://host?private=canary", "https://host?", "https://host#", "https://host#fragment"} {
		t.Run(endpoint, func(t *testing.T) {
			config := nativeBackendConfig(t)
			config.Clusters["cluster"].Server = endpoint
			_, err := parseNativeBackend(nativeBackendBytes(t, config))
			require.ErrorIs(t, err, errNativeBackend)
		})
	}
	for _, proxy := range []string{"/relative", "ftp://proxy.test", "http://user:password@proxy.test", "https://proxy.test?", "http://proxy.test?q=1", "http://proxy.test#fragment"} {
		t.Run(proxy, func(t *testing.T) {
			config := nativeBackendConfig(t)
			config.Clusters["cluster"].ProxyURL = proxy
			_, err := parseNativeBackend(nativeBackendBytes(t, config))
			require.ErrorIs(t, err, errNativeBackend)
		})
	}
	for _, proxy := range []string{"http://proxy.test:8080", "https://proxy.test", "socks5://proxy.test:1080"} {
		config := nativeBackendConfig(t)
		config.Clusters["cluster"].ProxyURL = proxy
		_, err := parseNativeBackend(nativeBackendBytes(t, config))
		require.NoError(t, err)
	}
}

func TestNativeBackendPreparationOwnsBoundedSnapshot(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "source")
	config := nativeBackendConfig(t)
	require.NoError(t, os.WriteFile(path, nativeBackendBytes(t, config), 0600))
	e := &Executor{KubernetesBackendConfigPath: path}
	require.NoError(t, e.PrepareNativeBackend())
	original := append([]byte(nil), e.NativeBackendIdentity()...)
	config.Clusters["cluster"].Server = "https://changed.test"
	require.NoError(t, os.WriteFile(path, nativeBackendBytes(t, config), 0600))
	require.Equal(t, original, []byte(e.NativeBackendIdentity()), "prepared bytes must not follow later source changes")
	copyOfIdentity := e.NativeBackendIdentity()
	copyOfIdentity[0] = '!'
	require.Equal(t, original, []byte(e.NativeBackendIdentity()))
	for _, test := range []struct {
		name string
		run  func(string)
	}{
		{"empty", func(p string) { require.NoError(t, os.WriteFile(p, nil, 0600)) }},
		{"oversized", func(p string) {
			require.NoError(t, os.WriteFile(p, bytes.Repeat([]byte("x"), nativeBackendLimit+1), 0600))
		}},
		{"public mode", func(p string) { require.NoError(t, os.WriteFile(p, []byte("x"), 0644)) }},
		{"directory", func(p string) { require.NoError(t, os.Mkdir(p, 0700)) }},
		{"FIFO", func(p string) { require.NoError(t, syscall.Mkfifo(p, 0600)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := filepath.Join(t.TempDir(), "input")
			test.run(bad)
			e.KubernetesBackendConfigPath = bad
			require.ErrorIs(t, e.PrepareNativeBackend(), errNativeBackend)
			require.Nil(t, e.nativeBackend)
		})
	}
}

func TestNativeBackendStagingAndSavedAuthenticationRefresh(t *testing.T) {
	config := nativeBackendConfig(t)
	prepared, err := parseNativeBackend(nativeBackendBytes(t, config))
	require.NoError(t, err)
	e := &Executor{nativeBackend: prepared}
	request := Request{RunnerUUID: "3cbdc8ce-60d5-4ccc-9900-c7b6ae874897", Backend: "kubernetes", Bundle: []byte("compiled")}
	dir := t.TempDir()
	require.NoError(t, e.stageNativeBackend(dir, request, false))
	path := filepath.Join(dir, nativeBackendFile)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Error(t, e.stageNativeBackend(dir, request, false), "compiled bundle collision must refuse")
	plan := filepath.Join(dir, "plan.tfplan")
	require.NoError(t, os.WriteFile(plan, []byte("immutable selected plan"), 0600))
	config.AuthInfos["user"].Token = "PRIVATE_BACKEND_REFRESHED_TOKEN"
	e.nativeBackend, err = parseNativeBackend(nativeBackendBytes(t, config))
	require.NoError(t, err)
	require.NoError(t, e.stageNativeBackend(dir, request, true))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEqual(t, before, after)
	require.Contains(t, string(after), "PRIVATE_BACKEND_REFRESHED_TOKEN")
	planBytes, err := os.ReadFile(plan)
	require.NoError(t, err)
	require.Equal(t, "immutable selected plan", string(planBytes))
	require.NoError(t, os.Chmod(path, 0400))
	require.ErrorIs(t, e.stageNativeBackend(dir, request, true), errNativeBackend, "write refusal must not expose the private path")
	require.NoError(t, os.Chmod(path, 0600))
	config.Clusters["cluster"].Server = "https://foreign.test"
	e.nativeBackend, err = parseNativeBackend(nativeBackendBytes(t, config))
	require.NoError(t, err)
	require.ErrorIs(t, e.stageNativeBackend(dir, request, true), errNativeBackend)
	retained, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, after, retained, "foreign destination must not overwrite retained auth")
	require.NotContains(t, string(retained), "foreign.test")
	request.RunnerUUID = ""
	require.NoError(t, e.stageNativeBackend(t.TempDir(), request, false), "legacy staging stays unchanged")
	require.False(t, strings.Contains(string(prepared.destination), "PRIVATE_BACKEND"))
}
