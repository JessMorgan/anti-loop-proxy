package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeYaml writes a YAML config file with the given content into dir
// and returns its path.
func writeYaml(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("writing yaml file: %v", err)
	}
	return p
}

// missingFile returns a path inside dir that is guaranteed not to exist,
// so Load does not pick up a real /etc/anti-loop-proxy/config.yaml.
func missingFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "does-not-exist.yaml")
}

func TestLoadDefaults(t *testing.T) {
	missing := missingFile(t)
	t.Setenv("ANTI_LOOP_CONFIG_FILE", missing)

	// Upstream is required, so a defaults-only load must fail validation.
	if _, err := Load(); err == nil {
		t.Fatal("Load() with no upstream expected error, got nil")
	}

	// Set a minimal valid upstream to check the remaining defaults.
	t.Setenv("ANTI_LOOP_UPSTREAM", "https://api.example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	want := &Config{
		Listen:     ":8080",
		Upstream:   "https://api.example.com",
		MinCount:   4,
		MinLen:     12,
		MaxLen:     200,
		MaxGap:     0,
		ConfigFile: missing,
		LogLevel:   "info",
	}
	if *cfg != *want {
		t.Errorf("config mismatch:\n got  %+v\n want %+v", *cfg, *want)
	}
}

func TestLoadYAMLOnly(t *testing.T) {
	dir := t.TempDir()
	p := writeYaml(t, dir, "config.yaml", `
listen: ":9090"
upstream: "https://yaml.example.com"
upstream_api_key: "yaml-key"
min_count: 3
min_len: 10
max_len: 150
max_gap: 7
log_level: "debug"
`)
	t.Setenv("ANTI_LOOP_CONFIG_FILE", p)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	want := &Config{
		Listen:         ":9090",
		Upstream:       "https://yaml.example.com",
		UpstreamAPIKey: "yaml-key",
		MinCount:       3,
		MinLen:         10,
		MaxLen:         150,
		MaxGap:         7,
		ConfigFile:     p,
		LogLevel:       "debug",
	}
	if *cfg != *want {
		t.Errorf("config mismatch:\n got  %+v\n want %+v", *cfg, *want)
	}
}

func TestLoadEnvOnly(t *testing.T) {
	missing := missingFile(t)
	t.Setenv("ANTI_LOOP_CONFIG_FILE", missing)
	t.Setenv("ANTI_LOOP_LISTEN", ":7070")
	t.Setenv("ANTI_LOOP_UPSTREAM", "https://env.example.com")
	t.Setenv("ANTI_LOOP_UPSTREAM_API_KEY", "env-key")
	t.Setenv("ANTI_LOOP_MIN_COUNT", "5")
	t.Setenv("ANTI_LOOP_MIN_LEN", "20")
	t.Setenv("ANTI_LOOP_MAX_LEN", "300")
	t.Setenv("ANTI_LOOP_MAX_GAP", "9")
	t.Setenv("ANTI_LOOP_LOG_LEVEL", "warn")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	want := &Config{
		Listen:         ":7070",
		Upstream:       "https://env.example.com",
		UpstreamAPIKey: "env-key",
		MinCount:       5,
		MinLen:         20,
		MaxLen:         300,
		MaxGap:         9,
		ConfigFile:     missing,
		LogLevel:       "warn",
	}
	if *cfg != *want {
		t.Errorf("config mismatch:\n got  %+v\n want %+v", *cfg, *want)
	}
}

func TestLoadEnvBeatsYAML(t *testing.T) {
	dir := t.TempDir()
	p := writeYaml(t, dir, "config.yaml", `
listen: ":9090"
upstream: "https://yaml.example.com"
upstream_api_key: "yaml-key"
min_count: 3
min_len: 10
max_len: 150
max_gap: 7
log_level: "debug"
`)
	t.Setenv("ANTI_LOOP_CONFIG_FILE", p)
	t.Setenv("ANTI_LOOP_LISTEN", ":7070")
	t.Setenv("ANTI_LOOP_UPSTREAM", "https://env.example.com")
	t.Setenv("ANTI_LOOP_UPSTREAM_API_KEY", "env-key")
	t.Setenv("ANTI_LOOP_MIN_COUNT", "5")
	t.Setenv("ANTI_LOOP_MIN_LEN", "20")
	t.Setenv("ANTI_LOOP_MAX_LEN", "300")
	t.Setenv("ANTI_LOOP_MAX_GAP", "9")
	t.Setenv("ANTI_LOOP_LOG_LEVEL", "warn")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	want := &Config{
		Listen:         ":7070",
		Upstream:       "https://env.example.com",
		UpstreamAPIKey: "env-key",
		MinCount:       5,
		MinLen:         20,
		MaxLen:         300,
		MaxGap:         9,
		ConfigFile:     p,
		LogLevel:       "warn",
	}
	if *cfg != *want {
		t.Errorf("config mismatch (env should win):\n got  %+v\n want %+v", *cfg, *want)
	}
}

func TestLoadMissingConfigFileIsNotAnError(t *testing.T) {
	t.Setenv("ANTI_LOOP_CONFIG_FILE", missingFile(t))
	t.Setenv("ANTI_LOOP_UPSTREAM", "https://api.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with missing config file returned error: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("expected default listen :8080, got %q", cfg.Listen)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring that must appear in the error
	}{
		{
			name:    "missing upstream",
			env:     map[string]string{},
			wantErr: "upstream",
		},
		{
			name: "upstream not absolute URL",
			env:  map[string]string{"ANTI_LOOP_UPSTREAM": "example.com"},
			wantErr: "upstream",
		},
		{
			name: "upstream not http/https",
			env:  map[string]string{"ANTI_LOOP_UPSTREAM": "ftp://example.com"},
			wantErr: "upstream",
		},
		{
			name:    "min_count < 2",
			env:     map[string]string{"ANTI_LOOP_MIN_COUNT": "1"},
			wantErr: "min_count",
		},
		{
			name:    "min_len < 1",
			env:     map[string]string{"ANTI_LOOP_MIN_LEN": "0"},
			wantErr: "min_len",
		},
		{
			name:    "max_len < min_len",
			env:     map[string]string{"ANTI_LOOP_MIN_LEN": "100", "ANTI_LOOP_MAX_LEN": "50"},
			wantErr: "max_len",
		},
		{
			name:    "max_len > 4096",
			env:     map[string]string{"ANTI_LOOP_MAX_LEN": "5000"},
			wantErr: "max_len",
		},
		{
			name:    "max_gap negative",
			env:     map[string]string{"ANTI_LOOP_MAX_GAP": "-1"},
			wantErr: "max_gap",
		},
		{
			name:    "bad log level",
			env:     map[string]string{"ANTI_LOOP_LOG_LEVEL": "verbose"},
			wantErr: "log_level",
		},
		{
			name:    "bad listen address",
			env:     map[string]string{"ANTI_LOOP_LISTEN": "1.2.3.4:5:6"},
			wantErr: "listen",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTI_LOOP_CONFIG_FILE", missingFile(t))
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			// Provide a valid upstream unless the case is about upstream.
			if _, ok := tc.env["ANTI_LOOP_UPSTREAM"]; !ok && tc.name != "missing upstream" {
				t.Setenv("ANTI_LOOP_UPSTREAM", "https://api.example.com")
			}

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadListenWithoutPortDefaultsTo8080(t *testing.T) {
	t.Setenv("ANTI_LOOP_CONFIG_FILE", missingFile(t))
	t.Setenv("ANTI_LOOP_UPSTREAM", "https://api.example.com")
	t.Setenv("ANTI_LOOP_LISTEN", "127.0.0.1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with host-only listen address returned error: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("expected listen %q, got %q", "127.0.0.1:8080", cfg.Listen)
	}
}
