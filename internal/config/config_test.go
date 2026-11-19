package config

import (
	"os"
	"path/filepath"
	"testing"
)

func loadTestConfig(t *testing.T, body string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func TestDefaultServerKeys(t *testing.T) {
	cfg := loadTestConfig(t, `{"listen":"127.0.0.1:0","anonymous":true}`)
	want := []string{"3178848", "123"}
	if len(cfg.ServerKeys) != len(want) {
		t.Fatalf("server keys = %#v, want %#v", cfg.ServerKeys, want)
	}
	for i := range want {
		if cfg.ServerKeys[i] != want[i] {
			t.Fatalf("server keys = %#v, want %#v", cfg.ServerKeys, want)
		}
	}
}

func TestShortConfiguredServerKeyIsKept(t *testing.T) {
	cfg := loadTestConfig(t, `{"listen":"127.0.0.1:0","anonymous":true,"server_keys":["123"]}`)
	if len(cfg.ServerKeys) != 1 || cfg.ServerKeys[0] != "123" {
		t.Fatalf("server keys = %#v, want [123]", cfg.ServerKeys)
	}
}
