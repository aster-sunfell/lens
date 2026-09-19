package gateway

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

	"github.com/aster-sunfell/lens/internal/config"
	"github.com/aster-sunfell/lens/internal/vision"
)

type Handler struct {
	cfg        config.Config
	vision     Analyzer
	textClient *http.Client
	logger     *slog.Logger
	requestSeq atomic.Uint64
}

func New(cfg config.Config, vision Analyzer, textClient *http.Client, logger *slog.Logger) *Handler {
	return &Handler{cfg: cfg, vision: vision, textClient: textClient, logger: logger}
}

func (g *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}

	requestID := strconv.FormatUint(g.requestSeq.Add(1), 36)
	started := time.Now()
	g.logger.Info("request received", "request_id", requestID, "method", r.Method, "path", r.URL.Path)
	kind, isGeneration := classifyGenerationRequest(r)
	var body io.Reader = r.Body
	contentLength := r.ContentLength
	imageCount := 0

	if isGeneration {
		if encoding := strings.TrimSpace(r.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
			g.logRequest(slog.LevelWarn, "request rejected", r, requestID, started, http.StatusUnsupportedMediaType, 0, "code", "unsupported_content_encoding")
			writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_content_encoding", "compressed generation requests are not supported")
			return
		}
		limited := http.MaxBytesReader(w, r.Body, g.cfg.MaxRequestBytes)
		rawBody, err := io.ReadAll(limited)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				g.logRequest(slog.LevelWarn, "request rejected", r, requestID, started, http.StatusRequestEntityTooLarge, 0, "code", "request_too_large")
				writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("request exceeds %d bytes", g.cfg.MaxRequestBytes))
			} else {
				g.logRequest(slog.LevelWarn, "request rejected", r, requestID, started, http.StatusBadRequest, 0, "code", "invalid_request_body")
				writeAPIError(w, http.StatusBadRequest, "invalid_request_body", "could not read request body")
			}
			return
		}
		root, err := decodeJSONObject(rawBody)
		if err != nil {
			g.logRequest(slog.LevelWarn, "request rejected", r, requestID, started, http.StatusBadRequest, 0, "code", "invalid_json")
			writeAPIError(w, http.StatusBadRequest, "invalid_json", "request body must be one JSON object")
			return
		}
		imageCount, err = TransformImages(r.Context(), root, kind, g.vision)
		if err != nil {
			var inputErr *ImageInputError
			var visionInputErr *vision.InputError
			if errors.As(err, &inputErr) || errors.As(err, &visionInputErr) {
				message := err.Error()
				if inputErr != nil {
					message = inputErr.Error()
				} else if visionInputErr != nil {
					message = visionInputErr.Error()
				}
				g.logRequest(slog.LevelWarn, "request rejected", r, requestID, started, http.StatusUnprocessableEntity, 0, "code", "unsupported_image")
				writeAPIError(w, http.StatusUnprocessableEntity, "unsupported_image", message)
			} else if !errors.Is(err, r.Context().Err()) {
				g.logRequest(slog.LevelError, "vision preprocessing failed", r, requestID, started, http.StatusBadGateway, 0, "error", safeLogError(err))
				writeAPIError(w, http.StatusBadGateway, "gateway_vision_error", "the vision upstream could not produce valid image evidence")
			} else {
				g.logRequest(slog.LevelInfo, "client disconnected during vision preprocessing", r, requestID, started, 0, 0)
			}
			return
		}
		if imageCount > 0 {
			g.logger.Info("vision preprocessing completed", "request_id", requestID, "images", imageCount, "duration_ms", time.Since(started).Milliseconds())
		}
		transformed, err := json.Marshal(root)
		if err != nil {
			g.logRequest(slog.LevelError, "could not encode transformed request", r, requestID, started, http.StatusInternalServerError, imageCount, "error", err)
			writeAPIError(w, http.StatusInternalServerError, "gateway_error", "could not encode transformed request")
			return
		}
		body = bytes.NewReader(transformed)
		contentLength = int64(len(transformed))
	}

	result, err := g.proxy(w, r, body, contentLength)
	status := result.status
	if status == 0 && err != nil {
		status = http.StatusBadGateway
	}
	fields := []any{"upstream_protocol", result.protocol, "stream_outcome", result.streamOutcome}
	if err != nil {
		if r.Context().Err() != nil && errors.Is(err, r.Context().Err()) {
			if result.streamOutcome == "completed" {
				g.logRequest(slog.LevelInfo, "client disconnected after response completion", r, requestID, started, status, imageCount, fields...)
			} else {
				g.logRequest(slog.LevelWarn, "client disconnected before response completion", r, requestID, started, status, imageCount, fields...)
			}
		} else {
			g.logRequest(slog.LevelError, "text upstream request failed", r, requestID, started, status, imageCount, append(fields, "error", safeLogError(err))...)
		}
		if result.status == 0 && r.Context().Err() == nil {
			writeAPIError(w, http.StatusBadGateway, "gateway_upstream_error", "the text upstream request failed")
		}
		return
	}
	switch {
	case status >= 500:
		g.logRequest(slog.LevelError, "text upstream returned error", r, requestID, started, status, imageCount, fields...)
	case result.streamOutcome == "failed" || result.streamOutcome == "missing_terminal":
		g.logRequest(slog.LevelError, "text upstream stream failed", r, requestID, started, status, imageCount, fields...)
	case status >= 400:
		g.logRequest(slog.LevelWarn, "text upstream returned error", r, requestID, started, status, imageCount, fields...)
	case result.streamOutcome == "incomplete":
		g.logRequest(slog.LevelWarn, "text upstream returned incomplete response", r, requestID, started, status, imageCount, fields...)
	default:
		g.logRequest(slog.LevelInfo, "request completed", r, requestID, started, status, imageCount, fields...)
	}
}

// net/http wraps request failures in url.Error, whose string includes the
// full URL (and potentially a client's API key in its query). Keep the
// underlying transport error for diagnosis without logging that URL.
func safeLogError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return safeLogError(urlErr.Err)
	}
	return err.Error()
}

func (g *Handler) logRequest(level slog.Level, message string, r *http.Request, requestID string, started time.Time, status, images int, fields ...any) {
	args := []any{
		"request_id", requestID,
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"images", images,
		"duration_ms", time.Since(started).Milliseconds(),
	}
	g.logger.Log(r.Context(), level, message, append(args, fields...)...)
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
