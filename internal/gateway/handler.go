package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
			var visionInputErr *vision.InputError
			if errors.As(err, &inputErr) || errors.As(err, &visionInputErr) {
				message := err.Error()
				if inputErr != nil {
					message = inputErr.Error()
				} else if visionInputErr != nil {
					message = visionInputErr.Error()
				}
				writeAPIError(w, http.StatusUnprocessableEntity, "unsupported_image", message)
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
