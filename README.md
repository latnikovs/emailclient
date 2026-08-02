# emailclient

Go client for a JSON email-sending API. It is a **standalone module** with
**stdlib-only dependencies**, so importing it never drags a server's cloud SDK
dependencies into your application.

- Import path: `github.com/latnikovs/emailclient`
- Package name: `emailclient`

## Install

```sh
go get github.com/latnikovs/emailclient@latest
```

## Usage

```go
package main

import (
	"context"
	"errors"
	"os"

	"github.com/latnikovs/emailclient"
)

func main() {
	ec, err := emailclient.New(os.Getenv("EMAIL_SERVICE_URL"), os.Getenv("EMAIL_API_KEY"))
	if err != nil {
		panic(err)
	}

	res, err := ec.Send(context.Background(), emailclient.SendRequest{
		From:           "noreply@app.example.com",
		To:             []string{"user@dest.com"},
		Subject:        "Welcome",
		Text:           "Hello!",
		IdempotencyKey: "signup-42", // makes the send safe to retry
	})
	switch {
	case errors.Is(err, emailclient.ErrRateLimited):
		// back off and retry later
	case err != nil:
		panic(err)
	default:
		_ = res.ID // queued message id (also the correlation/idempotency id)
	}
}
```

### Attachments

```go
data, _ := os.ReadFile("invoice.pdf")
req.Attachments = []emailclient.Attachment{
	emailclient.AttachFile("invoice.pdf", "application/pdf", data),
}
```

Pass an empty content type to sniff it from the bytes. Attachment bytes are
base64-encoded on the wire automatically; large payloads are offloaded server-side
transparently.

### Errors

`Send` returns an `*emailclient.APIError` for failure responses. Match the category
with `errors.Is` against `ErrUnauthorized`, `ErrRateLimited`, `ErrInvalidRequest`,
`ErrInvalidSenderDomain`, `ErrProviderFailure`, `ErrInternal`, or inspect
`APIError.StatusCode` / `Code` / `Message` directly.

### Retries

`Send` retries `429`, `5xx`, and transport errors with exponential backoff and
jitter, honouring `context` cancellation. Set `IdempotencyKey` so retries resolve
to the same message id instead of double-sending. Tune with:

- `WithMaxRetries(n)` — retry budget (default `3`; `0` disables).
- `WithTimeout(d)` — per-attempt timeout (default `30s`).
- `WithHTTPClient(hc)` — supply your own `*http.Client`.

## Integrating in your app

A few conventions keep usage consistent across services:

- **Construct once, at startup.** `Client` is safe for concurrent use; build it
  during wiring from your existing config/secrets (service URL + API key) and reuse
  it. Don't create one per request.

  ```go
  ec, err := emailclient.New(cfg.EmailServiceURL, cfg.EmailAPIKey)
  if err != nil {
      return fmt.Errorf("email client: %w", err)
  }
  svc := signup.NewService(repo, ec) // inject as a dependency
  ```

- **Inject behind a small interface** so use cases stay testable with a fake:

  ```go
  type Mailer interface {
      Send(ctx context.Context, req emailclient.SendRequest) (emailclient.SendResult, error)
  }
  ```

- **Always set a stable `IdempotencyKey`** derived from the triggering event
  (e.g. `"signup:" + user.ID`). The built-in retries — and any app-level retry —
  then resolve to the same message id instead of double-sending.

- **Branch on the error category**, not the HTTP status:

  ```go
  res, err := s.email.Send(ctx, emailclient.SendRequest{
      From:           "noreply@app.example.com",
      To:             []string{user.Email},
      Subject:        "Welcome",
      HTML:           welcomeHTML,
      IdempotencyKey: "signup:" + user.ID,
  })
  switch {
  case errors.Is(err, emailclient.ErrRateLimited):
      return err // transient; already auto-retried, back off at app level
  case errors.Is(err, emailclient.ErrInvalidSenderDomain):
      return err // config bug: this app isn't allowed to send from that domain
  case err != nil:
      return err
  }
  log.Info("queued", "message_id", res.ID)
  ```

Before this works, the server must have provisioned an API key for your app and
authorised the sender domains you intend to use. That is an operational step on the
server side, separate from this library.

## Releasing

```sh
git tag v0.1.0 && git push origin v0.1.0
```
