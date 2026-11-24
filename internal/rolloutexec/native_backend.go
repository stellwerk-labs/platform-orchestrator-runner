package rolloutexec

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const nativeBackendFile = ".stellwerk-kubernetes-backend.yaml"
const nativeBackendLimit = 1 << 20

var errNativeBackend = errors.New("native Kubernetes backend configuration refused")

type nativeBackend struct {
	snapshot    []byte
	destination []byte
	proxy       string
}

type nativeBackendDestination struct {
	Mode               string `json:"mode"`
	Server             string `json:"server,omitempty"`
	CertificateData    []byte `json:"certificate_authority_data,omitempty"`
	TLSServerName      string `json:"tls_server_name,omitempty"`
	ProxyURL           string `json:"proxy_url,omitempty"`
	DisableCompression bool   `json:"disable_compression,omitempty"`
}

// PrepareNativeBackend reads only the explicit configuration, without storage,
// credentials discovery, plugins or network access. Callers own this Executor.
func (e *Executor) PrepareNativeBackend() error {
	e.nativeBackend = nil
	if e.KubernetesBackendConfigPath == "" {
		return nil
	}
	data, err := readNativeBackendFile(e.KubernetesBackendConfigPath)
	if err != nil {
		return errNativeBackend
	}
	prepared, err := parseNativeBackend(data)
	if err != nil || runner.ValidateNativeKubernetesEnvironment(prepared.proxy) != nil {
		return errNativeBackend
	}
	e.nativeBackend = prepared
	return nil
}

// NativeBackendIdentity is private storage material, never public inventory.
func (e *Executor) NativeBackendIdentity() json.RawMessage {
	if e.nativeBackend == nil {
		return json.RawMessage(`{"mode":"none"}`)
	}
	return append(json.RawMessage(nil), e.nativeBackend.destination...)
}

func readNativeBackendFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errNativeBackend
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > nativeBackendLimit || info.Mode().Perm()&0007 != 0 {
		return nil, errNativeBackend
	}
	data, err := io.ReadAll(io.LimitReader(file, nativeBackendLimit+1))
	if err != nil || len(data) > nativeBackendLimit {
		return nil, errNativeBackend
	}
	return data, nil
}

func parseNativeBackend(data []byte) (*nativeBackend, error) {
	if len(data) == 0 || len(data) > nativeBackendLimit {
		return nil, errNativeBackend
	}
	config, err := clientcmd.Load(data)
	if err != nil || config.Contexts[config.CurrentContext] == nil || clientcmdapi.MinifyConfig(config) != nil || len(config.Clusters) != 1 || len(config.AuthInfos) != 1 ||
		len(config.Extensions) != 0 || len(config.Preferences.Extensions) != 0 {
		return nil, errNativeBackend
	}
	context := config.Contexts[config.CurrentContext]
	cluster, auth := config.Clusters[context.Cluster], config.AuthInfos[context.AuthInfo]
	if cluster == nil || auth == nil || len(context.Extensions) != 0 || len(cluster.Extensions) != 0 || len(auth.Extensions) != 0 ||
		cluster.CertificateAuthority != "" || cluster.InsecureSkipTLSVerify || !safeNativeBackendURL(cluster.Server, false) ||
		auth.ClientCertificate != "" || auth.ClientKey != "" || auth.TokenFile != "" || auth.Exec != nil || auth.AuthProvider != nil {
		return nil, errNativeBackend
	}
	roots := x509.NewCertPool()
	if len(cluster.CertificateAuthorityData) == 0 || !roots.AppendCertsFromPEM(cluster.CertificateAuthorityData) ||
		(cluster.ProxyURL != "" && !safeNativeBackendURL(cluster.ProxyURL, true)) {
		return nil, errNativeBackend
	}
	destination, err := json.Marshal(nativeBackendDestination{Mode: "kubernetes-inline/v1", Server: cluster.Server,
		CertificateData: cluster.CertificateAuthorityData, TLSServerName: cluster.TLSServerName,
		ProxyURL: cluster.ProxyURL, DisableCompression: cluster.DisableCompression})
	if err != nil {
		return nil, errNativeBackend
	}
	// Origin paths and aliases are not destination authority. Stable names also
	// make independently provisioned same-target worker snapshots portable.
	cluster.LocationOfOrigin, auth.LocationOfOrigin, context.LocationOfOrigin = "", "", ""
	context.Cluster, context.AuthInfo, context.Namespace = "backend", "backend", ""
	config.CurrentContext = "backend"
	config.Contexts = map[string]*clientcmdapi.Context{"backend": context}
	config.Clusters = map[string]*clientcmdapi.Cluster{"backend": cluster}
	config.AuthInfos = map[string]*clientcmdapi.AuthInfo{"backend": auth}
	snapshot, err := clientcmd.Write(*config)
	if err != nil || len(snapshot) > nativeBackendLimit {
		return nil, errNativeBackend
	}
	return &nativeBackend{snapshot: snapshot, destination: destination, proxy: cluster.ProxyURL}, nil
}

func safeNativeBackendURL(value string, proxy bool) bool {
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(value, "#") {
		return false
	}
	return parsed.Scheme == "https" || proxy && (parsed.Scheme == "http" || parsed.Scheme == "socks5")
}

func (e *Executor) validateNativeBackend(request Request) error {
	if request.RunnerUUID == "" || request.Backend != "kubernetes" {
		return nil
	}
	if e.nativeBackend == nil || len(request.Bundle) == 0 || runner.ValidateNativeKubernetesEnvironment(e.nativeBackend.proxy) != nil {
		return errNativeBackend
	}
	return nil
}

func (e *Executor) stageNativeBackend(dir string, request Request, restored bool) error {
	if request.RunnerUUID == "" || request.Backend != "kubernetes" {
		return nil
	}
	if err := e.validateNativeBackend(request); err != nil {
		return err
	}
	path := filepath.Join(dir, nativeBackendFile)
	if restored {
		retained, err := readNativeBackendFile(path)
		if err != nil {
			return errNativeBackend
		}
		previous, err := parseNativeBackend(retained)
		if err != nil || !bytes.Equal(previous.destination, e.nativeBackend.destination) {
			return errNativeBackend
		}
		if os.WriteFile(path, e.nativeBackend.snapshot, 0600) != nil {
			return errNativeBackend
		}
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errNativeBackend
	}
	_, writeErr := file.Write(e.nativeBackend.snapshot)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errNativeBackend
	}
	return nil
}
