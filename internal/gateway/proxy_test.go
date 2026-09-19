package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSSEObserverTracksTerminalAcrossChunks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    APIKind
		body    string
		outcome string
	}{
		{name: "responses completed", kind: APIResponses, body: "event: response.created\r\ndata: {}\r\n\r\nevent: response.completed\r\ndata: {}\r\n\r\n", outcome: "completed"},
		{name: "responses failed", kind: APIResponses, body: "event: response.failed\ndata: {}\n\n", outcome: "failed"},
		{name: "responses incomplete", kind: APIResponses, body: "event: response.incomplete\ndata: {}\n\n", outcome: "incomplete"},
		{name: "responses missing terminal", kind: APIResponses, body: "event: response.created\ndata: {}\n\n", outcome: "missing_terminal"},
		{name: "chat completed", kind: APIChatCompletions, body: "data: {\"choices\":[]}\n\ndata: [DONE]\n\n", outcome: "completed"},
		{name: "chat completed without space", kind: APIChatCompletions, body: "data:[DONE]\n\n", outcome: "completed"},
		{name: "chat error", kind: APIChatCompletions, body: "event: error\ndata: {}\n\n", outcome: "failed"},
		{name: "long data line", kind: APIResponses, body: "data: " + strings.Repeat("x", 8192) + "\n\nevent: response.completed\ndata: {}\n\n", outcome: "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observer := &sseObserver{kind: tc.kind}
			for offset := 0; offset < len(tc.body); offset += 3 {
				end := offset + 3
				if end > len(tc.body) {
					end = len(tc.body)
				}
				observer.observe([]byte(tc.body[offset:end]))
			}
			if got := observer.outcome(); got != tc.outcome {
				t.Fatalf("stream outcome = %q, want %q", got, tc.outcome)
			}
			if len(observer.line) > 4096 {
				t.Fatalf("observer buffered %d bytes", len(observer.line))
			}
		})
	}
}

func TestGatewayLogsClientCancellationBeforeCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL + "/v1")
	logs := &synchronizedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	gateway := httptest.NewServer(New(testConfig(base, base), &recordingResolver{}, upstream.Client(), logger))
	defer gateway.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(`{"model":"client-model","stream":true,"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var first [64]byte
	if _, err := response.Body.Read(first[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	response.Body.Close()

	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), `"msg":"client disconnected before response completion"`) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	logged := logs.String()
	if !strings.Contains(logged, `"level":"WARN"`) || !strings.Contains(logged, `"stream_outcome":"missing_terminal"`) {
		t.Fatalf("unexpected cancellation log: %s", logged)
	}
}

type synchronizedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestGatewayLogsUpstreamFailureWithoutLoggingResponseBody(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		body        string
		wantLevel   string
		wantMessage string
		wantOutcome string
	}{
		{name: "success", status: 200, contentType: "text/event-stream", body: "event: response.completed\ndata: {}\n\n", wantLevel: "INFO", wantMessage: "request completed", wantOutcome: "completed"},
		{name: "failed SSE event", status: 200, contentType: "text/event-stream", body: "event: response.failed\ndata: {\"error\":\"private upstream details\"}\n\n", wantLevel: "ERROR", wantMessage: "text upstream stream failed", wantOutcome: "failed"},
		{name: "missing SSE terminal", status: 200, contentType: "text/event-stream", body: "event: response.created\ndata: {}\n\n", wantLevel: "ERROR", wantMessage: "text upstream stream failed", wantOutcome: "missing_terminal"},
		{name: "upstream 502", status: 502, contentType: "application/json", body: "{\"error\":\"private upstream details\"}", wantLevel: "ERROR", wantMessage: "text upstream returned error"},
		{name: "upstream 401", status: 401, contentType: "application/json", body: "{\"error\":\"private upstream details\"}", wantLevel: "WARN", wantMessage: "text upstream returned error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			base, _ := url.Parse(upstream.URL + "/v1")
			logs := &synchronizedBuffer{}
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			gateway := httptest.NewServer(New(testConfig(base, base), &recordingResolver{}, upstream.Client(), logger))
			defer gateway.Close()

			response, err := http.Post(gateway.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"client-model","stream":true,"input":"hello"}`))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != tc.status || string(body) != tc.body {
				t.Fatalf("status=%d body=%q error=%v", response.StatusCode, body, err)
			}

			deadline := time.Now().Add(time.Second)
			for !strings.Contains(logs.String(), `"msg":"`+tc.wantMessage+`"`) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			logged := logs.String()
			if !strings.Contains(logged, `"level":"`+tc.wantLevel+`"`) || !strings.Contains(logged, `"msg":"`+tc.wantMessage+`"`) {
				t.Fatalf("unexpected log: %s", logged)
			}
			if tc.wantOutcome != "" && !strings.Contains(logged, `"stream_outcome":"`+tc.wantOutcome+`"`) {
				t.Fatalf("missing stream outcome in log: %s", logged)
			}
			if strings.Contains(logged, "private upstream details") {
				t.Fatalf("upstream response body leaked into log: %s", logged)
			}
		})
	}
}

func TestGatewayDoesNotLogClientQueryOnTransportFailure(t *testing.T) {
	base, _ := url.Parse("https://text.example.com/v1")
	logs := &synchronizedBuffer{}
	client := &http.Client{Transport: failingTransport{}}
	gateway := httptest.NewServer(New(testConfig(base, base), &recordingResolver{}, client, slog.New(slog.NewJSONHandler(logs, nil))))
	defer gateway.Close()

	response, err := http.Post(gateway.URL+"/v1/responses?api_key=private-query-token", "application/json", strings.NewReader(`{"model":"client-model","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.StatusCode)
	}
	logged := logs.String()
	if strings.Contains(logged, "private-query-token") || !strings.Contains(logged, "unexpected EOF") {
		t.Fatalf("transport error log must contain the cause but not the request query: %s", logged)
	}
}

func TestGatewayLogsUpstreamDisconnectAfterStreamStarts(t *testing.T) {
	base, _ := url.Parse("https://text.example.com/v1")
	logs := &synchronizedBuffer{}
	client := &http.Client{Transport: disconnectingStreamTransport{}}
	gateway := httptest.NewServer(New(testConfig(base, base), &recordingResolver{}, client, slog.New(slog.NewJSONHandler(logs, nil))))
	defer gateway.Close()

	response, err := http.Post(gateway.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"client-model","stream":true,"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), `"msg":"text upstream request failed"`) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	logged := logs.String()
	for _, want := range []string{`"level":"ERROR"`, `"status":200`, `"upstream_protocol":"HTTP/2.0"`, `"stream_outcome":"missing_terminal"`, "unexpected EOF"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("missing %q in stream disconnect log: %s", want, logged)
		}
	}
}

type disconnectingStreamTransport struct{}

func (disconnectingStreamTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/2.0",
		Header:        http.Header{"Content-Type": {"text/event-stream"}},
		Body:          io.NopCloser(io.MultiReader(strings.NewReader("event: response.created\ndata: {}\n\n"), unexpectedEOFReader{})),
		ContentLength: -1,
		Request:       request,
	}, nil
}

type unexpectedEOFReader struct{}

func (unexpectedEOFReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}
