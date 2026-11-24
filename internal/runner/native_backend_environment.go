package runner

import (
	"errors"
	"os"
	"strings"
)

func (r *baseRunner) validateNativeKubernetesEnvironment() error {
	if r.nativeKubernetesProxy != nil {
		return ValidateNativeKubernetesEnvironment(*r.nativeKubernetesProxy)
	}
	return nil
}

// ValidateNativeKubernetesEnvironment prevents ambient backend overrides from
// selecting a destination outside the prepared private Native profile.
func ValidateNativeKubernetesEnvironment(proxy string) error {
	for _, value := range os.Environ() {
		name, content, _ := strings.Cut(value, "=")
		if content == "" {
			continue
		}
		if strings.HasPrefix(name, "KUBE_") {
			return errors.New("native Kubernetes backend environment refused")
		}
		if proxy == "" {
			switch name {
			case "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy":
				return errors.New("native Kubernetes backend environment refused")
			}
		}
	}
	return nil
}
