// Package emailclient is a Go client for a JSON email-sending API.
//
// It is a standalone module (stdlib-only dependencies) so importing it never
// pulls a server's cloud SDK dependencies into your application.
//
// # Usage
//
//	ec, err := emailclient.New(os.Getenv("EMAIL_SERVICE_URL"), os.Getenv("EMAIL_API_KEY"))
//	if err != nil {
//		return err
//	}
//	res, err := ec.Send(ctx, emailclient.SendRequest{
//		From:           "noreply@app.example.com",
//		To:             []string{"user@dest.com"},
//		Subject:        "Welcome",
//		Text:           "Hello!",
//		IdempotencyKey: signupID, // makes the send safe to retry
//	})
//	if errors.Is(err, emailclient.ErrRateLimited) {
//		// back off and try later
//	}
//
// # Idempotency and retries
//
// Send automatically retries 429, 5xx, and transport errors with exponential
// backoff. Set SendRequest.IdempotencyKey so a retry resolves to the same message
// id rather than sending a duplicate.
//
// # Attachments
//
// Use AttachFile to build attachments; bytes are base64-encoded on the wire
// automatically. Large payloads are offloaded server-side transparently.
package emailclient
