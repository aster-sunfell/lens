package config

import (
	"strings"
	"testing"
)

func TestLoadDoesNotRequireTextCredentialsOrModel(t *testing.T) {
	for _, name := range []string{
		"LENS_LISTEN",
		"TEXT_HTTP_COMPATIBILITY_MODE",
		"MAX_REQUEST_BYTES",
		"MAX_IMAGE_BYTES",
		"VISION_TIMEOUT_SECONDS",
		"VISION_MAX_CONCURRENCY",
		"VISION_CACHE_TTL_SECONDS",
		"VISION_CACHE_MAX_ENTRIES",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("TEXT_BASE_URL", "https://text.example.com/v1")
	t.Setenv("VISION_BASE_URL", "https://vision.example.com/v1")
	t.Setenv("VISION_API_KEY", "vision-secret")
	t.Setenv("VISION_MODEL", "vision-model")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8787" {
		t.Fatalf("listen address = %q", cfg.ListenAddr)
	}
	if cfg.TextBaseURL.String() != "https://text.example.com/v1" {
		t.Fatalf("text base URL = %q", cfg.TextBaseURL)
	}
	if cfg.TextHTTPCompatibilityMode != TextHTTPModeHTTP2 {
		t.Fatalf("text HTTP compatibility mode = %q", cfg.TextHTTPCompatibilityMode)
	}
}

func TestLoadTextHTTPCompatibilityMode(t *testing.T) {
	t.Setenv("TEXT_BASE_URL", "https://text.example.com/v1")
	t.Setenv("VISION_BASE_URL", "https://vision.example.com/v1")
	t.Setenv("VISION_API_KEY", "vision-secret")
	t.Setenv("VISION_MODEL", "vision-model")
	for _, tc := range []struct {
		mode    string
		wantErr bool
	}{
		{mode: TextHTTPModeHTTP2},
		{mode: TextHTTPModeHTTP11},
		{mode: "auto", wantErr: true},
		{mode: "http3", wantErr: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("TEXT_HTTP_COMPATIBILITY_MODE", tc.mode)
			cfg, err := Load("")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "TEXT_HTTP_COMPATIBILITY_MODE") {
					t.Fatalf("expected invalid compatibility mode error, got %v", err)
				}
				return
			}
			if err != nil || cfg.TextHTTPCompatibilityMode != tc.mode {
				t.Fatalf("mode = %q, error = %v", cfg.TextHTTPCompatibilityMode, err)
			}
		})
	}
}
