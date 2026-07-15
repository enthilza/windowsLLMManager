package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

const ServiceName = "WindowsLLMManager"

type Config struct {
	ListenAddress           string `json:"listen_address"`
	TokenPath               string `json:"token_path"`
	TLSCertPath             string `json:"tls_cert_path"`
	TLSKeyPath              string `json:"tls_key_path"`
	TrustedProxyIP          string `json:"trusted_proxy_ip,omitempty"`
	MaxSessions             int    `json:"max_sessions"`
	IdleSessionTimeoutSec   int    `json:"idle_session_timeout_sec"`
	CommandTimeoutSec       int    `json:"command_timeout_sec"`
	MaxOutputBytes          int    `json:"max_output_bytes"`
	MaxRequestBytes         int64  `json:"max_request_bytes"`
	RateLimitPerSec         int    `json:"rate_limit_per_sec"`
	RateLimitBurst          int    `json:"rate_limit_burst"`
	AuthFailuresBeforeBlock int    `json:"auth_failures_before_block"`
	AuditLogPath            string `json:"audit_log_path"`
	AuditMaxBytes           int64  `json:"audit_max_bytes"`
	KillSwitchPath          string `json:"kill_switch_path"`
}

func Default(baseDir string) Config {
	return Config{
		ListenAddress:           "0.0.0.0:8443",
		TokenPath:               filepath.Join(baseDir, "token.txt"),
		TLSCertPath:             filepath.Join(baseDir, "tls-cert.pem"),
		TLSKeyPath:              filepath.Join(baseDir, "tls-key.pem"),
		MaxSessions:             5,
		IdleSessionTimeoutSec:   1800,
		CommandTimeoutSec:       120,
		MaxOutputBytes:          4 * 1024 * 1024,
		MaxRequestBytes:         1024 * 1024,
		RateLimitPerSec:         10,
		RateLimitBurst:          20,
		AuthFailuresBeforeBlock: 5,
		AuditLogPath:            filepath.Join(baseDir, "logs", "audit.jsonl"),
		AuditMaxBytes:           50 * 1024 * 1024,
		KillSwitchPath:          filepath.Join(baseDir, "KILLED"),
	}
}

func Load(path string) (Config, error) {
	baseDir := filepath.Dir(path)
	cfg := Default(baseDir)
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Config{}, errors.New("decode config: trailing JSON data")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.ListenAddress); err != nil {
		return fmt.Errorf("listen_address: %w", err)
	}
	if c.TrustedProxyIP != "" && net.ParseIP(c.TrustedProxyIP) == nil {
		return errors.New("trusted_proxy_ip must be an IP address")
	}
	if c.TokenPath == "" || c.TLSCertPath == "" || c.TLSKeyPath == "" || c.AuditLogPath == "" || c.KillSwitchPath == "" {
		return errors.New("token, TLS, audit and kill-switch paths are required")
	}
	if c.MaxSessions < 1 || c.MaxSessions > 100 {
		return errors.New("max_sessions must be between 1 and 100")
	}
	if c.IdleSessionTimeoutSec < 30 || c.CommandTimeoutSec < 1 {
		return errors.New("idle_session_timeout_sec must be >= 30 and command_timeout_sec >= 1")
	}
	if c.MaxOutputBytes < 1024 || c.MaxRequestBytes < 1024 {
		return errors.New("max_output_bytes and max_request_bytes must be >= 1024")
	}
	if c.RateLimitPerSec < 1 || c.RateLimitBurst < 1 || c.AuthFailuresBeforeBlock < 1 {
		return errors.New("rate and auth-failure limits must be positive")
	}
	if c.AuditMaxBytes < 1024*1024 {
		return errors.New("audit_max_bytes must be >= 1 MiB")
	}
	return nil
}

func (c Config) CommandTimeout() time.Duration {
	return time.Duration(c.CommandTimeoutSec) * time.Second
}
func (c Config) IdleTimeout() time.Duration {
	return time.Duration(c.IdleSessionTimeoutSec) * time.Second
}
