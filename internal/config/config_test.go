package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAcceptsUTF8BOMFromWindowsPowerShell(t *testing.T) {
	dir := t.TempDir()
	cfg := Default(dir)
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b = append([]byte{0xEF, 0xBB, 0xBF}, b...)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ListenAddress != cfg.ListenAddress {
		t.Fatalf("unexpected config: %+v", loaded)
	}
}

func TestLoadAddsUpdaterSchedulerDefaultsToLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := Default(dir)
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(b, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "update_check_interval_min")
	delete(legacy, "updater_path")
	delete(legacy, "updater_config_path")
	b, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UpdateCheckIntervalMin != 20 || loaded.UpdaterPath != filepath.Join(dir, "updater.exe") || loaded.UpdaterConfigPath != filepath.Join(dir, "updater-config.json") {
		t.Fatalf("legacy config did not receive updater defaults: %+v", loaded)
	}
}
