package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestResponseWriterWrapperUnwrapReturnsWrappedWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := NewResponseWriterWrapper(c.Writer, stubRequestLogger{}, &RequestInfo{}, c)
	if got := wrapper.Unwrap(); got != c.Writer {
		t.Fatal("expected Unwrap to return the wrapped response writer")
	}
}

func TestRequestLoggingMiddlewareAllowsStreamingPastServerWriteTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const writeTimeout = 100 * time.Millisecond
	r := gin.New()
	r.Use(RequestLoggingMiddleware(stubRequestLogger{}))
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		if err := http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{}); err != nil {
			c.String(http.StatusInternalServerError, "set write deadline: %v", err)
			return
		}

		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		if _, err := c.Writer.Write([]byte("data: first\n\n")); err != nil {
			return
		}
		c.Writer.Flush()

		time.Sleep(3 * writeTimeout)

		if _, err := c.Writer.Write([]byte("data: second\n\n")); err != nil {
			return
		}
		c.Writer.Flush()
	})

	server := httptest.NewUnstartedServer(r)
	server.Config.WriteTimeout = writeTimeout
	server.Start()
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewBufferString(`{"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("post streaming request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streaming response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, body)
	}
	if got := string(body); !strings.Contains(got, "data: first") || !strings.Contains(got, "data: second") {
		t.Fatalf("streaming body = %q, want both chunks", got)
	}
}

func TestExtractRequestBodyPrefersOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{
		requestInfo: &RequestInfo{Body: []byte("original-body")},
	}

	body := wrapper.extractRequestBody(c)
	if string(body) != "original-body" {
		t.Fatalf("request body = %q, want %q", string(body), "original-body")
	}

	c.Set(requestBodyOverrideContextKey, []byte("override-body"))
	body = wrapper.extractRequestBody(c)
	if string(body) != "override-body" {
		t.Fatalf("request body = %q, want %q", string(body), "override-body")
	}
}

func TestExtractRequestBodySupportsStringOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &ResponseWriterWrapper{}
	c.Set(requestBodyOverrideContextKey, "override-as-string")

	body := wrapper.extractRequestBody(c)
	if string(body) != "override-as-string" {
		t.Fatalf("request body = %q, want %q", string(body), "override-as-string")
	}
}
