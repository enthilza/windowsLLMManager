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
