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
	AllowedOrigins []string `json:"allowed_origins"`
	WorkspaceRoot  string   `json:"workspace_root"`
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
	return config, nil
}

func Save(config Config) error {
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
