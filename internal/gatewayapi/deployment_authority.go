package gatewayapi

import (
	"errors"
	"net/url"
	"path"
	"strings"
)

// ValidateDeploymentAuthorityURL checks an operator-controlled DP base URL.
func ValidateDeploymentAuthorityURL(value string) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("deployment authority URL must be an absolute HTTP or HTTPS URL")
	}
	if strings.TrimSpace(value) != value || strings.Contains(value, "\\") || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") || u.EscapedPath() != u.Path {
		return errors.New("deployment authority URL must not contain credentials, query, fragment or encoded paths")
	}
	if u.Path == "//" || (u.Path != "" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") && u.Path != "/") {
		return errors.New("deployment authority URL path must be canonical")
	}
	return nil
}

// ValidateDirectDeploymentAccess keeps protected Native execution fully local.
func ValidateDirectDeploymentAccess(authorityURL string, disabled, retainedEnabled bool) error {
	if disabled && (authorityURL != "" || retainedEnabled) {
		return errors.New("static native gateway cannot enable deployment authority lookup or retained-key forwarding")
	}
	return ValidateDeploymentAuthorityURL(authorityURL)
}
