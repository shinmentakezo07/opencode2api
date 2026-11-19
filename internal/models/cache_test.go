package models

import (
	"os"
	"path/filepath"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

func TestCatalogCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	source := Catalog(config.TierZen, nil)
	source.SetCachePath(path)
	source.ReplaceWithCapabilities(
		[]string{"zen-b", "zen-a", "zen-a"},
		[]string{"go-c"},
		map[config.Tier]map[string]wire.Protocol{
			config.TierZen: {"zen-a": wire.Anthropic},
			config.TierGo:  {"go-c": wire.Chat},
		},
		map[config.Tier]map[string]bool{config.TierGo: {"go-c": true}},
		nil,
	)
	if err := source.SaveCache(); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("cache mode = %o, want 600", perm)
	}

	loaded := Catalog(config.TierZen, nil)
	if err := loaded.LoadCache(path); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	snapshot := loaded.Snapshot()
	if snapshot.Zen != 2 || snapshot.Go != 1 || snapshot.Total != 3 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if !snapshot.Stale || snapshot.CacheSource != "disk" {
		t.Fatalf("snapshot = %#v, want stale disk cache", snapshot)
	}
	route, err := loaded.RouteForTier("zen-a", config.TierZen, true, false)
	if err != nil {
		t.Fatalf("RouteForTier: %v", err)
	}
	if route.ProtocolFor(config.TierZen) != wire.Anthropic {
		t.Fatalf("protocol = %v, want anthropic", route.ProtocolFor(config.TierZen))
	}
}

func TestCatalogCacheRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"schema":  `{"schema_version":999,"updated_at":"2026-01-01T00:00:00Z","zen":["a"]}`,
		"empty":   `{"schema_version":4,"updated_at":"2026-01-01T00:00:00Z"}`,
		"no-time": `{"schema_version":4,"zen":["a"]}`,
		"extra":   `{"schema_version":4,"updated_at":"2026-01-01T00:00:00Z","zen":["a"]} {"x":1}`,
	}
	for name, body := range cases {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadModelCatalogCache(path); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}
