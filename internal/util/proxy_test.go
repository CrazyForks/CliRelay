package util

import (
	"net/http"
	"testing"
	"time"
)

func TestNewDefaultTransportUsesLongResponseHeaderTimeout(t *testing.T) {
	transport := NewDefaultTransport(false)
	if transport == nil {
		t.Fatal("NewDefaultTransport returned nil")
	}
	if transport.ResponseHeaderTimeout != 600*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %s, want 600s", transport.ResponseHeaderTimeout)
	}
}

func TestBuildProxyTransportUsesLongResponseHeaderTimeout(t *testing.T) {
	transport := BuildProxyTransport("http://127.0.0.1:8080", false)
	if transport == nil {
		t.Fatal("BuildProxyTransport returned nil")
	}
	if transport.Proxy == nil {
		t.Fatal("Proxy is nil")
	}
	if transport.ResponseHeaderTimeout != 600*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %s, want 600s", transport.ResponseHeaderTimeout)
	}

	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy returned error: %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://127.0.0.1:8080" {
		t.Fatalf("Proxy URL = %v, want http://127.0.0.1:8080", proxyURL)
	}
}
