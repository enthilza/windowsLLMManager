package updater

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"windowsllmmanager/internal/config"
)

type Config struct {
	GitHubOwner       string `json:"github_owner"`
	GitHubRepository  string `json:"github_repository"`
	AgentPath         string `json:"agent_path"`
	ServiceName       string `json:"service_name"`
	KillSwitchPath    string `json:"kill_switch_path"`
	LogPath           string `json:"log_path"`
	CheckTimeoutSec   int    `json:"check_timeout_sec"`
	ServiceTimeoutSec int    `json:"service_timeout_sec"`
	AgentAssetName    string `json:"agent_asset_name"`
}

func Default(baseDir string) Config {
	return Config{
		AgentPath: filepath.Join(baseDir, "agent.exe"), ServiceName: config.ServiceName,
		KillSwitchPath: filepath.Join(baseDir, "KILLED"), LogPath: filepath.Join(baseDir, "logs", "updater.log"),
		CheckTimeoutSec: 60, ServiceTimeoutSec: 30, AgentAssetName: "agent.exe",
	}
}

func Load(path string) (Config, error) {
	cfg := Default(filepath.Dir(path))
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode updater config: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Config{}, errors.New("decode updater config: trailing JSON data")
	}
	if cfg.GitHubOwner == "" || cfg.GitHubRepository == "" {
		return Config{}, errors.New("github_owner and github_repository are required")
	}
	if cfg.AgentPath == "" || cfg.ServiceName == "" || cfg.KillSwitchPath == "" || cfg.LogPath == "" {
		return Config{}, errors.New("agent, service, kill-switch and log settings are required")
	}
	if cfg.CheckTimeoutSec < 10 || cfg.ServiceTimeoutSec < 5 {
		return Config{}, errors.New("check_timeout_sec must be >= 10 and service_timeout_sec >= 5")
	}
	return cfg, nil
}

func (c Config) CheckTimeout() time.Duration { return time.Duration(c.CheckTimeoutSec) * time.Second }
func (c Config) ServiceTimeout() time.Duration {
	return time.Duration(c.ServiceTimeoutSec) * time.Second
}
