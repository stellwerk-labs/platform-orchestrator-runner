package runner

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func validateRetainedProviderConfig(tool string) error {
	for _, name := range []string{"TF_REATTACH_PROVIDERS", "TOFU_REATTACH_PROVIDERS", "TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE", "TOFU_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE"} {
		if os.Getenv(name) != "" {
			return errors.New("retained provider override unsupported")
		}
	}
	paths, err := providerCLIConfigPaths(tool)
	if err != nil {
		return err
	}
	return validateProviderCLIConfigFiles(paths)
}

func validateProviderCLIConfigFiles(paths []string) error {
	for _, path := range paths {
		data, err := readProviderCLIConfig(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || len(data) > 1<<20 {
			return errors.New("retained provider configuration unavailable")
		}
		var value any
		if json.Unmarshal(data, &value) == nil {
			data, _ = json.Marshal(value)
		}
		if unsupportedProviderConfig(string(data)) {
			return errors.New("retained provider override unsupported")
		}
	}
	return nil
}

func unsupportedProviderConfig(data string) bool {
	if unsupportedProviderSetting(data) {
		return true
	}
	for {
		index := strings.IndexByte(data, '"')
		if index < 0 {
			return false
		}
		quoted, err := strconv.QuotedPrefix(data[index:])
		if err != nil {
			return true
		}
		value, err := strconv.Unquote(quoted)
		if err != nil || unsupportedProviderSetting(value) {
			return true
		}
		data = data[index+len(quoted):]
	}
}

func unsupportedProviderSetting(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "dev_overrides") || strings.Contains(value, "plugin_cache_may_break_dependency_lock_file")
}

func providerCLIConfigPaths(tool string) ([]string, error) {
	for _, name := range []string{"TF_CLI_CONFIG_FILE", "TERRAFORM_CONFIG"} {
		if value := os.Getenv(name); value != "" {
			return []string{value}, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return defaultProviderCLIConfigPaths(tool, home, os.Getenv("XDG_CONFIG_HOME"))
}

func defaultProviderCLIConfigPaths(tool, directory, xdg string) ([]string, error) {
	main := filepath.Join(directory, ".terraformrc")
	configDir := filepath.Join(directory, ".terraform.d")
	if tool == "opentofu" {
		var err error
		main, configDir, err = tofuCLIConfigLocations(directory, xdg)
		if err != nil {
			return nil, err
		}
	}
	paths := []string{main}
	entries, err := os.ReadDir(configDir)
	if os.IsNotExist(err) {
		return paths, nil
	}
	if err != nil {
		return nil, errors.New("provider configuration directory unavailable")
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tfrc") || strings.HasSuffix(entry.Name(), ".tfrc.json") {
			paths = append(paths, filepath.Join(configDir, entry.Name()))
		}
		if len(paths) > 64 {
			return nil, errors.New("provider configuration limit exceeded")
		}
	}
	return paths, nil
}

func tofuCLIConfigLocations(directory, xdg string) (string, string, error) {
	main := filepath.Join(directory, ".tofurc")
	legacy := filepath.Join(directory, ".terraformrc")
	configDir := filepath.Join(directory, ".terraform.d")
	for _, path := range []string{main, legacy, configDir} {
		if _, err := os.Stat(path); err != nil && !os.IsNotExist(err) {
			return "", "", errors.New("provider configuration location unavailable")
		}
	}
	if _, err := os.Stat(main); os.IsNotExist(err) {
		main = legacy
		if _, err = os.Stat(legacy); os.IsNotExist(err) && xdg != "" {
			main = filepath.Join(xdg, "opentofu", "tofurc")
		}
	}
	if _, err := os.Stat(configDir); os.IsNotExist(err) && xdg != "" {
		configDir = filepath.Join(xdg, "opentofu")
	}
	return main, configDir, nil
}

func readProviderCLIConfig(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("unsupported provider configuration")
	}
	return io.ReadAll(io.LimitReader(file, (1<<20)+1))
}
