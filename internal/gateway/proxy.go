package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type proxyResult struct {
	status        int
	protocol      string
	streamOutcome string
}

func (g *Handler) proxy(w http.ResponseWriter, source *http.Request, body io.Reader, contentLength int64) (proxyResult, error) {
	target := proxyURL(g.cfg.TextBaseURL, source.URL)
	request, err := http.NewRequestWithContext(source.Context(), source.Method, target.String(), body)
	if err != nil {
		return proxyResult{}, err
	}
	copyHeaders(request.Header, source.Header)
	removeHopByHopHeaders(request.Header)
	request.Header.Del("Content-Length")
	request.Host = target.Host
	request.ContentLength = contentLength

	response, err := g.textClient.Do(request)
	if err != nil {
		return proxyResult{}, err
	}
	defer response.Body.Close()
	result := proxyResult{status: response.StatusCode, protocol: response.Proto}
	var observer *sseObserver
	if kind, isGeneration := classifyGenerationRequest(source); isGeneration && strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		observer = &sseObserver{kind: kind}
	}
	copyHeaders(w.Header(), response.Header)
	removeHopByHopHeaders(w.Header())
	w.WriteHeader(response.StatusCode)
	err = copyAndFlush(w, response.Body, observer)
	if observer != nil {
		result.streamOutcome = observer.outcome()
	}
	return result, err
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

func copyAndFlush(destination http.ResponseWriter, source io.Reader, observer *sseObserver) error {
	buffer := make([]byte, 32<<10)
	controller := http.NewResponseController(destination)
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			written, writeErr := destination.Write(buffer[:n])
			if observer != nil {
				observer.observe(buffer[:written])
			}
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			if flushErr := controller.Flush(); flushErr != nil && !errors.Is(flushErr, http.ErrNotSupported) {
				return flushErr
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// sseObserver only reads event names and the chat completion marker. It never
// records response data, so streaming remains transparent and memory is bounded.
type sseObserver struct {
	kind         APIKind
	line         []byte
	skippingLine bool
	event        string
	done         bool
	terminal     string
}

func (s *sseObserver) observe(chunk []byte) {
	for _, b := range chunk {
		if b == '\n' {
			if s.skippingLine {
				s.skippingLine = false
				s.line = s.line[:0]
				continue
			}
			line := strings.TrimSpace(string(s.line))
			s.line = s.line[:0]
			switch {
			case line == "":
				s.finishEvent()
			case strings.HasPrefix(line, "event:"):
				s.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case s.kind == APIChatCompletions && strings.HasPrefix(line, "data:") && strings.TrimSpace(strings.TrimPrefix(line, "data:")) == "[DONE]":
				s.done = true
			}
		} else if !s.skippingLine {
			if len(s.line) < 4096 {
				s.line = append(s.line, b)
			} else {
				s.skippingLine = true
				s.line = s.line[:0]
			}
		}
	}
}

func (s *sseObserver) finishEvent() {
	switch {
	case s.event == "response.completed", s.kind == APIChatCompletions && s.done:
		s.terminal = "completed"
	case s.event == "response.failed", s.event == "error":
		s.terminal = "failed"
	case s.event == "response.incomplete":
		s.terminal = "incomplete"
	}
	s.event = ""
	s.done = false
}

func (s *sseObserver) outcome() string {
	if s.terminal == "" {
		return "missing_terminal"
	}
	return s.terminal
}
