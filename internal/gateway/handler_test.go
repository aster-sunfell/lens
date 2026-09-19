package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"lens/internal/config"
	"lens/internal/vision"
)

func TestGatewayTransformsResponsesRequestAndStreamsTextResponse(t *testing.T) {
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": validEvidenceJSON()}}},
		})
	}))
	defer visionServer.Close()

	var mu sync.Mutex
	var upstreamBody []byte
	var upstreamPath, upstreamAuth string
	textServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBody = body
		upstreamPath = r.URL.Path
		upstreamAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer textServer.Close()

	textBase, _ := url.Parse(textServer.URL + "/v1")
	visionBase, _ := url.Parse(visionServer.URL + "/v1")
	cfg := testConfig(textBase, visionBase)
	visionClient := vision.NewClient(cfg, visionServer.Client())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gatewayServer := httptest.NewServer(New(cfg, visionClient, textServer.Client(), logger))
	defer gatewayServer.Close()

	payload := `{
      "model":"client-model",
      "stream":true,
      "input":[{"role":"user","content":[
        {"type":"input_text","text":"Why did this fail?"},
        {"type":"input_image","image_url":"data:image/png;base64,aW1n"}
      ]}],
      "tools":[{"type":"function","name":"unchanged","parameters":{"type":"object"}}]
    }`
	req, _ := http.NewRequest(http.MethodPost, gatewayServer.URL+"/v1/responses", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer local-placeholder")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "[DONE]") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, responseBody)
	}

	mu.Lock()
	defer mu.Unlock()
	if upstreamPath != "/v1/responses" {
		t.Fatalf("upstream path = %q", upstreamPath)
	}
	if upstreamAuth != "Bearer text-secret" {
		t.Fatalf("upstream auth = %q", upstreamAuth)
	}
	if bytes.Contains(upstreamBody, []byte("data:image")) {
		t.Fatalf("raw image reached text upstream: %s", upstreamBody)
	}
	var transformed map[string]any
	if err := json.Unmarshal(upstreamBody, &transformed); err != nil {
		t.Fatal(err)
	}
	if transformed["model"] != "text-model" {
		t.Fatalf("model = %v", transformed["model"])
	}
	input := transformed["input"].([]any)
	content := input[0].(map[string]any)["content"].([]any)
	imageReplacement := content[1].(map[string]any)
	if imageReplacement["type"] != "input_text" || !strings.Contains(imageReplacement["text"].(string), "vision_evidence") {
		t.Fatalf("replacement = %#v", imageReplacement)
	}
	tool := transformed["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "unchanged" {
		t.Fatal("tool definition changed")
	}
}

func TestGatewayRejectsUnsupportedFileIDBeforeTextUpstream(t *testing.T) {
	var textCalls int
	textServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { textCalls++ }))
	defer textServer.Close()
	base, _ := url.Parse(textServer.URL)
	cfg := testConfig(base, base)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gateway := httptest.NewServer(New(cfg, &recordingResolver{}, textServer.Client(), logger))
	defer gateway.Close()

	body := `{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file-1"}]}]}`
	resp, err := http.Post(gateway.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, responseBody)
	}
	if textCalls != 0 {
		t.Fatalf("text upstream calls = %d", textCalls)
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
		VisionTimeout:         time.Second,
		VisionMaxConcurrency:  2,
		VisionCacheTTL:        time.Hour,
		VisionCacheMaxEntries: 16,
	}
}
