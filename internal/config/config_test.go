package config

import "testing"

func TestLoadDoesNotRequireTextCredentialsOrModel(t *testing.T) {
	for _, name := range []string{
		"LENS_LISTEN",
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
}
