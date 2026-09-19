package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aster-sunfell/lens/internal/config"
)

func TestTextTransportCompatibilityModes(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	defer server.Close()

	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())
	base := &http.Transport{
		DialContext:       (&net.Dialer{Timeout: time.Second}).DialContext,
		TLSClientConfig:   &tls.Config{RootCAs: rootCAs},
		ForceAttemptHTTP2: true,
	}
	for _, tc := range []struct {
		mode     string
		protocol int
	}{
		{mode: config.TextHTTPModeHTTP2, protocol: 2},
		{mode: config.TextHTTPModeHTTP11, protocol: 1},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			transport := newTextTransport(base, tc.mode)
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport}
			response, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			if response.ProtoMajor != tc.protocol {
				t.Fatalf("negotiated HTTP/%d, want HTTP/%d", response.ProtoMajor, tc.protocol)
			}
		})
	}
	if !base.ForceAttemptHTTP2 {
		t.Fatal("text compatibility mode changed the shared transport")
	}
	visionTransport := base.Clone()
	defer visionTransport.CloseIdleConnections()
	visionResponse, err := (&http.Client{Transport: visionTransport}).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	visionResponse.Body.Close()
	if visionResponse.ProtoMajor != 2 {
		t.Fatalf("shared transport negotiated HTTP/%d after text HTTP/1.1 mode", visionResponse.ProtoMajor)
	}
}

func TestTextTransportSupportsHTTP2OnlyUpstream(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())
	base := &http.Transport{
		DialContext:       (&net.Dialer{Timeout: time.Second}).DialContext,
		TLSClientConfig:   &tls.Config{RootCAs: rootCAs},
		ForceAttemptHTTP2: true,
	}
	transport := newTextTransport(base, config.TextHTTPModeHTTP2)
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Fatalf("negotiated HTTP/%d, want HTTP/2", response.ProtoMajor)
	}
}

func TestTextTransportHTTP2ModeSupportsHTTP11OnlyUpstream(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())
	base := &http.Transport{
		DialContext:       (&net.Dialer{Timeout: time.Second}).DialContext,
		TLSClientConfig:   &tls.Config{RootCAs: rootCAs},
		ForceAttemptHTTP2: true,
	}
	transport := newTextTransport(base, config.TextHTTPModeHTTP2)
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 1 {
		t.Fatalf("negotiated HTTP/%d, want HTTP/1.1", response.ProtoMajor)
	}
}
