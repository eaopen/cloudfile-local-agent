package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	AllowedOrigins []string   `json:"allowed_origins"`
	WorkspaceRoot  string     `json:"workspace_root"`
	OpenRules      []OpenRule `json:"open_rules"`
}

// OpenRule selects a locally configured program for one file extension and
// session mode.  It is local-only: a CloudFile session descriptor never gets
// to choose a program or supply command-line arguments.
type OpenRule struct {
	ID         string   `json:"id"`
	Modes      []string `json:"modes"`
	Extensions []string `json:"extensions"`
	Command    []string `json:"command"`
}

func FilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "CloudFileLocal", "config.json"), nil
}

func Load() (Config, error) {
	path, err := FilePath()
	if err != nil {
		return Config{}, err
	}
	config := Config{}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return config, nil
	}
	if err != nil {
		return Config{}, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func Save(config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	path, err := FilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

func (c Config) Validate() error {
	for _, origin := range c.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") ||
			origin != strings.TrimSuffix(u.Scheme+"://"+u.Host, "/") {
			return fmt.Errorf("invalid trusted origin: %q", origin)
		}
	}
	for index, rule := range c.OpenRules {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("invalid open rule %d: %w", index+1, err)
		}
	}
	return nil
}

func (r OpenRule) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("id is required")
	}
	if len(r.Modes) == 0 || len(r.Extensions) == 0 || len(r.Command) == 0 {
		return fmt.Errorf("modes, extensions and command are required")
	}
	for _, mode := range r.Modes {
		if mode != "local-view" && mode != "local-edit" {
			return fmt.Errorf("unsupported mode %q", mode)
		}
	}
	for _, extension := range r.Extensions {
		if extension != "*" && !isExtension(extension) {
			return fmt.Errorf("invalid extension %q", extension)
		}
	}
	if !filepath.IsAbs(r.Command[0]) {
		return fmt.Errorf("command executable must be an absolute path")
	}
	fileArguments := 0
	for _, argument := range r.Command {
		fileArguments += strings.Count(argument, "{file}")
	}
	if fileArguments != 1 {
		return fmt.Errorf("command must contain exactly one {file} placeholder")
	}
	return nil
}

func isExtension(value string) bool {
	if value == "" || strings.HasPrefix(value, ".") {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '+' || character == '-') {
			return false
		}
	}
	return value == strings.ToLower(value)
}

// ResolveOpenRule returns the first matching rule.  Rule order is therefore
// deliberate: place a file-specific rule before a wildcard fallback.
func (c Config) ResolveOpenRule(mode, name string) (OpenRule, bool) {
	extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	for _, rule := range c.OpenRules {
		if contains(rule.Modes, mode) && (contains(rule.Extensions, extension) || contains(rule.Extensions, "*")) {
			return rule, true
		}
	}
	return OpenRule{}, false
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (c Config) Allows(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	origin := u.Scheme + "://" + u.Host
	for _, allowed := range c.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

func (c Config) Root() (string, error) {
	if c.WorkspaceRoot != "" {
		return filepath.Abs(c.WorkspaceRoot)
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "CloudFileLocal", "sessions"), nil
}

func AddOrigin(rawOrigin string) (Config, error) {
	u, err := url.Parse(rawOrigin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return Config{}, fmt.Errorf("origin must be an http or https origin")
	}
	origin := strings.TrimSuffix(u.Scheme+"://"+u.Host, "/")
	config, err := Load()
	if err != nil {
		return Config{}, err
	}
	for _, existing := range config.AllowedOrigins {
		if existing == origin {
			return config, nil
		}
	}
	config.AllowedOrigins = append(config.AllowedOrigins, origin)
	return config, Save(config)
}
