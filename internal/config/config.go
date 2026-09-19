package config

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr            string
	TextBaseURL           *url.URL
	VisionBaseURL         *url.URL
	VisionAPIKey          string
	VisionModel           string
	MaxRequestBytes       int64
	MaxImageBytes         int64
	VisionTimeout         time.Duration
	VisionMaxConcurrency  int
	VisionCacheTTL        time.Duration
	VisionCacheMaxEntries int
}

func Load(dotEnvPath string) (Config, error) {
	if dotEnvPath != "" {
		if err := loadDotEnv(dotEnvPath); err != nil {
			return Config{}, err
		}
	}

	textURL, err := requiredURL("TEXT_BASE_URL", os.Getenv("TEXT_BASE_URL"))
	if err != nil {
		return Config{}, err
	}
	visionURL, err := requiredURL("VISION_BASE_URL", os.Getenv("VISION_BASE_URL"))
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		ListenAddr:            envOr("LENS_LISTEN", "127.0.0.1:8787"),
		TextBaseURL:           textURL,
		VisionBaseURL:         visionURL,
		VisionAPIKey:          os.Getenv("VISION_API_KEY"),
		VisionModel:           os.Getenv("VISION_MODEL"),
		MaxRequestBytes:       envInt64("MAX_REQUEST_BYTES", 64<<20),
		MaxImageBytes:         envInt64("MAX_IMAGE_BYTES", 20<<20),
		VisionTimeout:         time.Duration(envInt("VISION_TIMEOUT_SECONDS", 120)) * time.Second,
		VisionMaxConcurrency:  envInt("VISION_MAX_CONCURRENCY", 3),
		VisionCacheTTL:        time.Duration(envInt("VISION_CACHE_TTL_SECONDS", 3600)) * time.Second,
		VisionCacheMaxEntries: envInt("VISION_CACHE_MAX_ENTRIES", 256),
	}

	var missing []string
	for _, required := range []struct {
		name  string
		value string
	}{
		{"VISION_API_KEY", cfg.VisionAPIKey},
		{"VISION_MODEL", cfg.VisionModel},
	} {
		if strings.TrimSpace(required.value) == "" {
			missing = append(missing, required.name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	if cfg.MaxRequestBytes <= 0 || cfg.MaxImageBytes <= 0 || cfg.VisionTimeout <= 0 || cfg.VisionMaxConcurrency <= 0 || cfg.VisionCacheTTL <= 0 || cfg.VisionCacheMaxEntries <= 0 {
		return Config{}, errors.New("numeric limits and timeouts must be positive")
	}
	return cfg, nil
}

func requiredURL(name, value string) (*url.URL, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("missing required configuration: %s", name)
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%s must be an absolute http(s) URL", name)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s must not contain a query or fragment", name)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("set %s from %s: %w", key, path, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return parsed
}

func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return -1
	}
	return parsed
}
