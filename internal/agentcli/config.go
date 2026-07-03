package agentcli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type config struct {
	APIURL string `json:"api_url"`
	Token  string `json:"token"`
}

func configPath(homeDir string) (string, error) {
	if homeDir == "" {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(homeDir, ".roundtable-agent", "config.json"), nil
}

func saveConfig(homeDir string, cfg config) error {
	path, err := configPath(homeDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func loadConfig(homeDir string) (config, error) {
	path, err := configPath(homeDir)
	if err != nil {
		return config{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config{}, err
	}
	if cfg.APIURL == "" || cfg.Token == "" {
		return config{}, errors.New("agent profile is incomplete")
	}
	return cfg, nil
}
