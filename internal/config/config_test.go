package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:9999","api_key":"from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LISTEN", "127.0.0.1:1234")
	t.Setenv("MODELS", "a, b c")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:1234" {
		t.Fatalf("环境变量未覆盖: %q", cfg.Listen)
	}
	if cfg.APIKey != "from-file" {
		t.Fatalf("文件值应保留: %q", cfg.APIKey)
	}
	if len(cfg.Models) != 3 {
		t.Fatalf("MODELS 解析: %v", cfg.Models)
	}
	if cfg.Upstream.LLMBase == "" || cfg.Pool.MaxRotate == 0 {
		t.Fatalf("默认值没填上: %+v", cfg)
	}
}

func TestMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("配置文件不存在不该报错: %v", err)
	}
	if cfg.Listen != Default().Listen {
		t.Fatalf("应回落到默认 listen: %q", cfg.Listen)
	}
}

func TestBadJSONFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(path, []byte(`{oops`), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatalf("坏 JSON 应报错")
	}
}
