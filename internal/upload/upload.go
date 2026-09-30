// Package upload sends lifecycle events and OTLP batches to gauger-server.
package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mach4-braai/gauger/internal/metrics"
)

// Tokens hands out a bearer token for each request.
type Tokens interface {
	Token(ctx context.Context) (string, error)
}

// Client talks to one gauger-server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Tokens  Tokens
}

// StatusError is a non-2xx response.
type StatusError struct {
	Path string
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("POST %s: HTTP %d: %s", e.Path, e.Code, e.Body)
}

// Permanent reports whether retrying the same request cannot succeed. Auth,
// timeouts and rate limits can clear up, so they are not permanent.
func (e *StatusError) Permanent() bool {
	switch e.Code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return e.Code >= 400 && e.Code < 500
}

// Lifecycle is the JSON body of /v1/jobs/start and /v1/jobs/done.
type Lifecycle struct {
	Attributes map[string]string `json:"attributes"`
	Time       time.Time         `json:"time"`
	// UnsentBatches and DroppedBatches are set on done. A non-zero
	// UnsentBatches means the rest is in the fallback artifact.
	UnsentBatches  *int `json:"unsent_batches,omitempty"`
	DroppedBatches *int `json:"dropped_batches,omitempty"`
}

// NewLifecycle returns a lifecycle body for the identity attributes.
func NewLifecycle(attrs []metrics.Attribute, t time.Time) Lifecycle {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value
	}
	return Lifecycle{Attributes: m, Time: t.UTC()}
}

// Start posts /v1/jobs/start.
func (c *Client) Start(ctx context.Context, body Lifecycle) error {
	return c.postJSON(ctx, "/v1/jobs/start", body)
}

// Done posts /v1/jobs/done.
func (c *Client) Done(ctx context.Context, body Lifecycle) error {
	return c.postJSON(ctx, "/v1/jobs/done", body)
}

// Metrics posts one encoded ExportMetricsServiceRequest to /v1/metrics.
func (c *Client) Metrics(ctx context.Context, batch []byte) error {
	return c.post(ctx, "/v1/metrics", "application/x-protobuf", batch)
}

func (c *Client) postJSON(ctx context.Context, path string, body Lifecycle) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.post(ctx, path, "application/json", data)
}

func (c *Client) post(ctx context.Context, path, contentType string, body []byte) error {
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Path: path, Code: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	return nil
}
