package emailclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

const sendPath = "/v1/send"

// Client sends transactional email through the central email service. It is safe
// for concurrent use by multiple goroutines.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	maxRetries int
	baseDelay  time.Duration
	maxBackoff time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the underlying *http.Client. Use this to control
// transport, timeouts, or proxies. It overrides any WithTimeout applied earlier.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithTimeout sets the per-request timeout on the default HTTP client. This
// timeout covers a single attempt; retries each get the full timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// WithMaxRetries sets how many times a retryable failure (429, 5xx, or transport
// error) is retried before giving up. Zero disables retries. Retries are safe to
// enable when callers set SendRequest.IdempotencyKey.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// New builds a Client targeting baseURL (e.g. https://email.example.com)
// and authenticating with apiKey. It returns an error if either is empty.
func New(baseURL, apiKey string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("emailclient: baseURL is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("emailclient: apiKey is required")
	}
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		maxRetries: 3,
		baseDelay:  200 * time.Millisecond,
		maxBackoff: 30 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// SendRequest is a single transactional email. At least one of HTML or Text must
// be set. The field names mirror the service wire contract exactly; the service
// rejects unknown fields.
type SendRequest struct {
	From        string       `json:"from"`
	To          []string     `json:"to"`
	Subject     string       `json:"subject"`
	HTML        string       `json:"html,omitempty"`
	Text        string       `json:"text,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`

	// IdempotencyKey, when set, is sent as the Idempotency-Key header (not in the
	// body). A retry with the same key resolves to the same message id, making the
	// send safe to repeat. Must be visible ASCII, at most 255 characters.
	IdempotencyKey string `json:"-"`
}

// Attachment is a file delivered alongside the email body. Content holds the raw
// bytes; they are base64-encoded on the wire automatically.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Content     []byte `json:"content"`
}

// AttachFile builds an Attachment, sniffing the content type from the bytes when
// contentType is empty.
func AttachFile(filename, contentType string, content []byte) Attachment {
	if contentType == "" {
		contentType = http.DetectContentType(content)
	}
	return Attachment{Filename: filename, ContentType: contentType, Content: content}
}

// SendResult is the service's acknowledgement. Delivery is asynchronous, so ID is
// the queued message id (also the idempotency/correlation id), not a provider id.
type SendResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Send submits the email. On success it returns the queued message id. On a
// failure response it returns an *APIError (match it with errors.Is against the
// package sentinels). Retryable failures (429, 5xx, transport errors) are retried
// with exponential backoff and jitter, honouring ctx cancellation.
func (c *Client) Send(ctx context.Context, req SendRequest) (SendResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return SendResult{}, fmt.Errorf("emailclient: encode request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.wait(ctx, c.backoff(attempt)); err != nil {
				return SendResult{}, err
			}
		}
		res, retryable, err := c.do(ctx, body, req.IdempotencyKey)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !retryable {
			return SendResult{}, err
		}
	}
	return SendResult{}, lastErr
}

// do performs a single attempt. It returns whether the failure is retryable.
func (c *Client) do(ctx context.Context, body []byte, idempotencyKey string) (SendResult, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+sendPath, bytes.NewReader(body))
	if err != nil {
		return SendResult{}, false, fmt.Errorf("emailclient: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	if idempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// Transport error: retry unless the context was cancelled.
		return SendResult{}, ctx.Err() == nil, fmt.Errorf("emailclient: request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusAccepted {
		var sr SendResult
		if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
			return SendResult{}, false, fmt.Errorf("emailclient: decode response: %w", err)
		}
		return sr, false, nil
	}

	apiErr := decodeError(resp)
	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	return SendResult{}, retryable, apiErr
}

// decodeError reads the {error, message} envelope into an *APIError, falling back
// to the raw status when the body is missing or unparseable.
func decodeError(resp *http.Response) *APIError {
	var env struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error == "" {
		env.Error = "unknown"
		env.Message = http.StatusText(resp.StatusCode)
	}
	return &APIError{StatusCode: resp.StatusCode, Code: env.Error, Message: env.Message}
}

// backoff returns the delay before retry number attempt (1-based): exponential
// growth capped at maxBackoff, with full jitter to avoid thundering herds.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.baseDelay << (attempt - 1)
	if d <= 0 || d > c.maxBackoff {
		d = c.maxBackoff
	}
	return d/2 + time.Duration(rand.Int64N(int64(d)/2+1))
}

// wait sleeps for d or returns early if ctx is cancelled.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
