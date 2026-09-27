// Package config loads anti-loop-proxy configuration from a YAML file
// and environment variables. Precedence: env > file > default.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config holds the runtime configuration for the anti-loop proxy.
type Config struct {
	Listen         string // env ANTI_LOOP_LISTEN, yaml "listen", default ":8080"
	Upstream       string // env ANTI_LOOP_UPSTREAM, yaml "upstream", REQUIRED (no default)
	UpstreamAPIKey string // env ANTI_LOOP_UPSTREAM_API_KEY, yaml "upstream_api_key", default ""
	MinCount       int    // env ANTI_LOOP_MIN_COUNT, yaml "min_count", default 4
	MinLen         int    // env ANTI_LOOP_MIN_LEN, yaml "min_len", default 12
	MaxLen         int    // env ANTI_LOOP_MAX_LEN, yaml "max_len", default 200
	MaxGap         int    // env ANTI_LOOP_MAX_GAP, yaml "max_gap", default 0
	ConfigFile     string // env ANTI_LOOP_CONFIG_FILE only, default "/etc/anti-loop-proxy/config.yaml"
	LogLevel       string // env ANTI_LOOP_LOG_LEVEL, yaml "log_level", default "info"
}

// fileConfig is the YAML representation of the config file.
type fileConfig struct {
	Listen         string `yaml:"listen"`
	Upstream       string `yaml:"upstream"`
	UpstreamAPIKey string `yaml:"upstream_api_key"`
	MinCount       int    `yaml:"min_count"`
	MinLen         int    `yaml:"min_len"`
	MaxLen         int    `yaml:"max_len"`
	MaxGap         int    `yaml:"max_gap"`
	LogLevel       string `yaml:"log_level"`
}

const defaultConfigFile = "/etc/anti-loop-proxy/config.yaml"

// validLogLevels is the set of accepted log level values.
var validLogLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

// Load builds a Config from defaults, an optional YAML file, and
// environment variables (env > file > default), then validates it.
func Load() (*Config, error) {
	cfg := defaults()

	// Resolve config file path.
	if envFile := os.Getenv("ANTI_LOOP_CONFIG_FILE"); envFile != "" {
		cfg.ConfigFile = envFile
	}

	// Load YAML file if present. Missing file is not an error.
	paths := []string{cfg.ConfigFile}
	if os.Getenv("ANTI_LOOP_CONFIG_FILE") == "" {
		paths = append(paths, "./config.yaml")
	}
	for _, p := range paths {
		if fileCfg, err := loadFile(p); err == nil {
			overlayFile(cfg, fileCfg)
			break
		}
	}

	// Overlay environment variables.
	if v := os.Getenv("ANTI_LOOP_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("ANTI_LOOP_UPSTREAM"); v != "" {
		cfg.Upstream = v
	}
	if v := os.Getenv("ANTI_LOOP_UPSTREAM_API_KEY"); v != "" {
		cfg.UpstreamAPIKey = v
	}
	if v := os.Getenv("ANTI_LOOP_MIN_COUNT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: ANTI_LOOP_MIN_COUNT: invalid integer %q: %w", v, err)
		}
		cfg.MinCount = n
	}
	if v := os.Getenv("ANTI_LOOP_MIN_LEN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: ANTI_LOOP_MIN_LEN: invalid integer %q: %w", v, err)
		}
		cfg.MinLen = n
	}
	if v := os.Getenv("ANTI_LOOP_MAX_LEN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: ANTI_LOOP_MAX_LEN: invalid integer %q: %w", v, err)
		}
		cfg.MaxLen = n
	}
	if v := os.Getenv("ANTI_LOOP_MAX_GAP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: ANTI_LOOP_MAX_GAP: invalid integer %q: %w", v, err)
		}
		cfg.MaxGap = n
	}
	if v := os.Getenv("ANTI_LOOP_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}

	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Listen:     ":8080",
		MinCount:   4,
		MinLen:     12,
		MaxLen:     200,
		MaxGap:     0,
		ConfigFile: defaultConfigFile,
		LogLevel:   "info",
	}
}

// loadFile reads and parses a YAML config file.
func loadFile(path string) (*fileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fc fileConfig
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	return &fc, nil
}

// overlayFile applies non-empty YAML values onto cfg.
func overlayFile(cfg *Config, fc *fileConfig) {
	if fc.Listen != "" {
		cfg.Listen = fc.Listen
	}
	if fc.Upstream != "" {
		cfg.Upstream = fc.Upstream
	}
	if fc.UpstreamAPIKey != "" {
		cfg.UpstreamAPIKey = fc.UpstreamAPIKey
	}
	if fc.MinCount != 0 {
		cfg.MinCount = fc.MinCount
	}
	if fc.MinLen != 0 {
		cfg.MinLen = fc.MinLen
	}
	if fc.MaxLen != 0 {
		cfg.MaxLen = fc.MaxLen
	}
	if fc.MaxGap != 0 {
		cfg.MaxGap = fc.MaxGap
	}
	if fc.LogLevel != "" {
		cfg.LogLevel = fc.LogLevel
	}
}

// validate checks cfg and returns a descriptive error naming the
// offending field.
func validate(cfg *Config) error {
	// Upstream: required, absolute http(s) URL with host.
	if cfg.Upstream == "" {
		return fmt.Errorf("config: upstream: required field is empty")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil {
		return fmt.Errorf("config: upstream: invalid URL %q: %w", cfg.Upstream, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: upstream: must be an absolute http:// or https:// URL with a host, got %q", cfg.Upstream)
	}

	// MinCount.
	if cfg.MinCount < 2 {
		return fmt.Errorf("config: min_count: must be >= 2, got %d", cfg.MinCount)
	}

	// MinLen.
	if cfg.MinLen < 1 {
		return fmt.Errorf("config: min_len: must be >= 1, got %d", cfg.MinLen)
	}

	// MaxLen.
	if cfg.MaxLen < cfg.MinLen {
		return fmt.Errorf("config: max_len: must be >= min_len (%d), got %d", cfg.MinLen, cfg.MaxLen)
	}
	if cfg.MaxLen > 4096 {
		return fmt.Errorf("config: max_len: must be <= 4096, got %d", cfg.MaxLen)
	}

	// MaxGap.
	if cfg.MaxGap < 0 {
		return fmt.Errorf("config: max_gap: must be >= 0, got %d", cfg.MaxGap)
	}

	// LogLevel.
	if !validLogLevels[cfg.LogLevel] {
		return fmt.Errorf("config: log_level: must be one of debug, info, warn, error, got %q", cfg.LogLevel)
	}

	// Listen: valid host:port address; a bare host defaults to port 8080.
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		if !isMissingPort(err) {
			return fmt.Errorf("config: listen: invalid address %q: %w", cfg.Listen, err)
		}
		// No port given; default to 8080 and re-validate.
		cfg.Listen = cfg.Listen + ":8080"
		if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
			return fmt.Errorf("config: listen: invalid address %q: %w", cfg.Listen, err)
		}
	}

	return nil
}

// isMissingPort reports whether err is a net.AddrError caused by a
// missing port.
func isMissingPort(err error) bool {
	var ae *net.AddrError
	return errors.As(err, &ae) && ae.Err == "missing port in address"
}
