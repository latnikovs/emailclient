package emailclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points a Client at srv with near-zero backoff so retry tests run
// fast.
func newTestClient(t *testing.T, srv *httptest.Server, opts ...Option) *Client {
	t.Helper()
	c, err := New(srv.URL, "test-key", opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.baseDelay = time.Millisecond
	c.maxBackoff = 2 * time.Millisecond
	return c
}

func TestNewValidation(t *testing.T) {
	if _, err := New("", "k"); err == nil {
		t.Error("expected error for empty baseURL")
	}
	if _, err := New("http://x", ""); err == nil {
		t.Error("expected error for empty apiKey")
	}
	c, err := New("http://x/", "k")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.baseURL != "http://x" {
		t.Errorf("baseURL trailing slash not trimmed: %q", c.baseURL)
	}
}

func TestSendSuccessAndWireContract(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/send" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "idem-123" {
			t.Errorf("Idempotency-Key = %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("server decode: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"msg-1","status":"queued"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	res, err := c.Send(context.Background(), SendRequest{
		From:           "noreply@example.com",
		To:             []string{"u@dest.com"},
		Subject:        "Hi",
		Text:           "Hello",
		Attachments:    []Attachment{{Filename: "a.txt", ContentType: "text/plain", Content: []byte("hello")}},
		IdempotencyKey: "idem-123",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ID != "msg-1" || res.Status != "queued" {
		t.Errorf("result = %+v", res)
	}

	// Exact field names the server expects (DisallowUnknownFields on the server).
	for _, k := range []string{"from", "to", "subject", "text", "attachments"} {
		if _, ok := gotBody[k]; !ok {
			t.Errorf("body missing field %q; got %v", k, gotBody)
		}
	}
	// IdempotencyKey must NOT appear in the body.
	if _, ok := gotBody["IdempotencyKey"]; ok {
		t.Error("IdempotencyKey leaked into body")
	}
	// Attachment content must be base64 of the raw bytes.
	att := gotBody["attachments"].([]any)[0].(map[string]any)
	if att["content"] != "aGVsbG8=" {
		t.Errorf("attachment content not base64: %v", att["content"])
	}
	if att["filename"] != "a.txt" || att["content_type"] != "text/plain" {
		t.Errorf("attachment fields wrong: %v", att)
	}
}

func TestSendOmitsIdempotencyHeaderWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Idempotency-Key"]; ok {
			t.Error("Idempotency-Key should be absent")
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"x","status":"queued"}`))
	}))
	defer srv.Close()
	if _, err := newTestClient(t, srv).Send(context.Background(), SendRequest{To: []string{"a@b.c"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestSendErrorMapping(t *testing.T) {
	cases := []struct {
		status   int
		code     string
		sentinel error
	}{
		{http.StatusUnauthorized, "unauthorized", ErrUnauthorized},
		{http.StatusBadRequest, "invalid_request", ErrInvalidRequest},
		{http.StatusBadRequest, "invalid_sender_domain", ErrInvalidSenderDomain},
		{http.StatusBadGateway, "ses_failure", ErrProviderFailure},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.code, "message": "nope"})
			}))
			defer srv.Close()

			// 502 is retryable; keep retries off so we assert the final mapped error.
			c := newTestClient(t, srv, WithMaxRetries(0))
			_, err := c.Send(context.Background(), SendRequest{To: []string{"a@b.c"}})
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("errors.Is(%v, sentinel) = false", err)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status || apiErr.Code != tc.code {
				t.Fatalf("APIError = %+v", apiErr)
			}
		})
	}
}

func TestSendRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate_limited", "message": "slow down"})
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal_error", "message": "try again"})
		default:
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"ok","status":"queued"}`))
		}
	}))
	defer srv.Close()

	res, err := newTestClient(t, srv).Send(context.Background(), SendRequest{To: []string{"a@b.c"}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ID != "ok" {
		t.Errorf("result = %+v", res)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", calls.Load())
	}
}

func TestSendRetryBudgetExhausted(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate_limited", "message": "slow down"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, WithMaxRetries(2))
	_, err := c.Send(context.Background(), SendRequest{To: []string{"a@b.c"}})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
	if calls.Load() != 3 { // initial + 2 retries
		t.Errorf("expected 3 calls, got %d", calls.Load())
	}
}

func TestSendDoesNotRetry4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request", "message": "bad"})
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).Send(context.Background(), SendRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("4xx must not retry, got %d calls", calls.Load())
	}
}

func TestSendContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal_error", "message": "down"})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call
	_, err := newTestClient(t, srv).Send(ctx, SendRequest{To: []string{"a@b.c"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestAttachFileSniffsContentType(t *testing.T) {
	got := AttachFile("note.txt", "", []byte("plain text content"))
	if got.ContentType == "" {
		t.Error("expected sniffed content type")
	}
	explicit := AttachFile("doc.pdf", "application/pdf", []byte("x"))
	if explicit.ContentType != "application/pdf" {
		t.Errorf("explicit content type overridden: %q", explicit.ContentType)
	}
}
