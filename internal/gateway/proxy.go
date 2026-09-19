package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (g *Handler) proxy(w http.ResponseWriter, source *http.Request, body io.Reader, contentLength int64) (int, error) {
	target := proxyURL(g.cfg.TextBaseURL, source.URL)
	request, err := http.NewRequestWithContext(source.Context(), source.Method, target.String(), body)
	if err != nil {
		return 0, err
	}
	copyHeaders(request.Header, source.Header)
	removeHopByHopHeaders(request.Header)
	request.Header.Del("Content-Length")
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
