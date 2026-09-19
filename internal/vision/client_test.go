package vision

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aster-sunfell/lens/internal/config"
)

func TestVisionClientCallsCompatibleEndpointAndCaches(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer vision-secret" {
			t.Errorf("authorization was not replaced")
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "text-secret") {
			t.Error("text upstream secret leaked to vision upstream")
		}
		if !strings.Contains(string(body), "data:image/png;base64,aW1n") {
			t.Error("image missing from vision request")
		}
		responseContent := "```json\n" + validEvidenceJSON() + "\n```"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": responseContent}}},
		})
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL + "/v1")
	cfg := testConfig(base, base)
	client := NewClient(cfg, server.Client())
	for range 2 {
		evidence, err := client.Analyze(context.Background(), "data:image/png;base64,aW1n", "What failed?")
		if err != nil {
			t.Fatal(err)
		}
		if evidence.VisibleText != "invalid redirect_uri" {
			t.Fatalf("visible text = %q", evidence.VisibleText)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("vision calls = %d, want 1", calls.Load())
	}
}

func TestVisionClientRetriesInvalidEvidenceOnce(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		content := `{"description":"missing arrays"}`
		if call == 2 {
			content = validEvidenceJSON()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL)
	client := NewClient(testConfig(base, base), server.Client())
	if _, err := client.Analyze(context.Background(), "data:image/png;base64,aW1n", "focus"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("vision calls = %d, want 2", calls.Load())
	}
}

func validEvidenceJSON() string {
	return `{"description":"A login error dialog","visible_text":"invalid redirect_uri","relevant_details":["callback uses localhost"],"uncertainties":[]}`
}

func testConfig(textBase, visionBase *url.URL) config.Config {
	return config.Config{
		ListenAddr:            "127.0.0.1:0",
		TextBaseURL:           textBase,
		TextAPIKey:            "text-secret",
		TextModel:             "text-model",
		VisionBaseURL:         visionBase,
		VisionAPIKey:          "vision-secret",
		VisionModel:           "vision-model",
		MaxRequestBytes:       1 << 20,
		MaxImageBytes:         1 << 20,
		VisionTimeout:         10 * time.Second,
		VisionMaxConcurrency:  2,
		VisionCacheTTL:        time.Hour,
		VisionCacheMaxEntries: 16,
	}
}
