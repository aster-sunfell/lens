package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Gateway struct {
	cfg        Config
	vision     VisionResolver
	textClient *http.Client
	logger     *slog.Logger
	requestSeq atomic.Uint64
}

func NewGateway(cfg Config, vision VisionResolver, textClient *http.Client, logger *slog.Logger) *Gateway {
	return &Gateway{cfg: cfg, vision: vision, textClient: textClient, logger: logger}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}

	requestID := strconv.FormatUint(g.requestSeq.Add(1), 36)
	started := time.Now()
	kind, isGeneration := classifyGenerationRequest(r)
	var body io.Reader = r.Body
	contentLength := r.ContentLength
	imageCount := 0

	if isGeneration {
		if encoding := strings.TrimSpace(r.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
			writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_content_encoding", "compressed generation requests are not supported")
			return
		}
		limited := http.MaxBytesReader(w, r.Body, g.cfg.MaxRequestBytes)
		rawBody, err := io.ReadAll(limited)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("request exceeds %d bytes", g.cfg.MaxRequestBytes))
			} else {
				writeAPIError(w, http.StatusBadRequest, "invalid_request_body", "could not read request body")
			}
			return
		}
		root, err := decodeJSONObject(rawBody)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_json", "request body must be one JSON object")
			return
		}
		imageCount, err = TransformImages(r.Context(), root, kind, g.vision)
		if err != nil {
			var inputErr *ImageInputError
			if errors.As(err, &inputErr) {
				writeAPIError(w, http.StatusUnprocessableEntity, "unsupported_image", inputErr.Error())
			} else if !errors.Is(err, r.Context().Err()) {
				g.logger.Error("vision preprocessing failed", "request_id", requestID, "error", err)
				writeAPIError(w, http.StatusBadGateway, "gateway_vision_error", "the vision upstream could not produce valid image evidence")
			}
			return
		}
		root["model"] = g.cfg.TextModel
		transformed, err := json.Marshal(root)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "gateway_error", "could not encode transformed request")
			return
		}
		body = bytes.NewReader(transformed)
		contentLength = int64(len(transformed))
	}

	status, err := g.proxy(w, r, body, contentLength)
	if err != nil {
		g.logger.Error("text upstream request failed", "request_id", requestID, "error", err)
		if status == 0 {
			writeAPIError(w, http.StatusBadGateway, "gateway_upstream_error", "the text upstream request failed")
		}
		return
	}
	g.logger.Info("request completed",
		"request_id", requestID,
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"images", imageCount,
		"duration_ms", time.Since(started).Milliseconds(),
	)
}

func classifyGenerationRequest(r *http.Request) (APIKind, bool) {
	if r.Method != http.MethodPost {
		return 0, false
	}
	path := strings.TrimRight(r.URL.Path, "/")
	switch {
	case strings.HasSuffix(path, "/responses"):
		return APIResponses, true
	case strings.HasSuffix(path, "/chat/completions"):
		return APIChatCompletions, true
	default:
		return 0, false
	}
}

func (g *Gateway) proxy(w http.ResponseWriter, source *http.Request, body io.Reader, contentLength int64) (int, error) {
	target := proxyURL(g.cfg.TextBaseURL, source.URL)
	request, err := http.NewRequestWithContext(source.Context(), source.Method, target.String(), body)
	if err != nil {
		return 0, err
	}
	copyHeaders(request.Header, source.Header)
	removeHopByHopHeaders(request.Header)
	request.Header.Del("Content-Length")
	request.Header.Set("Authorization", "Bearer "+g.cfg.TextAPIKey)
	request.Host = target.Host
	request.ContentLength = contentLength

	response, err := g.textClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header)
	removeHopByHopHeaders(w.Header())
	w.WriteHeader(response.StatusCode)
	if err := copyAndFlush(w, response.Body); err != nil {
		return response.StatusCode, err
	}
	return response.StatusCode, nil
}

func proxyURL(base *url.URL, incoming *url.URL) *url.URL {
	target := *base
	basePath := strings.TrimRight(target.Path, "/")
	requestPath := incoming.Path
	if strings.HasSuffix(basePath, "/v1") && (requestPath == "/v1" || strings.HasPrefix(requestPath, "/v1/")) {
		requestPath = strings.TrimPrefix(requestPath, "/v1")
	}
	target.Path = basePath + "/" + strings.TrimLeft(requestPath, "/")
	target.RawPath = ""
	target.RawQuery = incoming.RawQuery
	return &target
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func removeHopByHopHeaders(header http.Header) {
	if connection := header.Get("Connection"); connection != "" {
		for _, name := range strings.Split(connection, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}

func copyAndFlush(destination http.ResponseWriter, source io.Reader) error {
	buffer := make([]byte, 32<<10)
	controller := http.NewResponseController(destination)
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if _, writeErr := destination.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
			_ = controller.Flush()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "gateway_error",
			"code":    code,
			"message": message,
		},
	})
}
