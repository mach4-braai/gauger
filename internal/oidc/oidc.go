// Package oidc fetches GitHub Actions OIDC tokens and refreshes them before they expire.
package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// RefreshBefore is how long before exp a token is replaced.
	RefreshBefore = 60 * time.Second
	// Retries is how many times a failed fetch is retried before giving up.
	Retries = 3
)

// Source hands out a token for one audience, fetching a new one when the
// cached token is within RefreshBefore of its exp.
type Source struct {
	RequestURL   string
	RequestToken string
	Audience     string
	HTTP         *http.Client
	Now          func() time.Time
	// Backoff is the wait before retry n, counting from 1.
	Backoff func(n int) time.Duration

	mu    sync.Mutex
	token string
	exp   time.Time
}

// FromEnv builds a Source from the variables the runner sets when the job has
// `id-token: write`. It fails when they are missing.
func FromEnv(getenv func(string) string, audience string) (*Source, error) {
	u, t := getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if u == "" || t == "" {
		return nil, errors.New("ACTIONS_ID_TOKEN_REQUEST_URL is not set; the job needs `permissions: id-token: write`")
	}
	return &Source{RequestURL: u, RequestToken: t, Audience: audience}, nil
}

// Token returns a token that is valid for at least RefreshBefore. If every
// fetch fails but the cached token has not expired yet, it returns that token.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.token != "" && now.Before(s.exp.Add(-RefreshBefore)) {
		return s.token, nil
	}
	var err error
	for attempt := 0; attempt <= Retries; attempt++ {
		if attempt > 0 {
			if werr := wait(ctx, s.backoff(attempt)); werr != nil {
				err = errors.Join(err, werr)
				break
			}
		}
		var token string
		var exp time.Time
		token, exp, err = s.fetch(ctx)
		if err == nil {
			s.token, s.exp = token, exp
			return token, nil
		}
	}
	if s.token != "" && s.now().Before(s.exp) {
		return s.token, nil
	}
	return "", fmt.Errorf("fetch OIDC token: %w", err)
}

func (s *Source) fetch(ctx context.Context) (string, time.Time, error) {
	u, err := url.Parse(s.RequestURL)
	if err != nil {
		return "", time.Time{}, err
	}
	q := u.Query()
	q.Set("audience", s.Audience)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "bearer "+s.RequestToken)
	req.Header.Set("Accept", "application/json")
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", time.Time{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("token endpoint returned %s", resp.Status)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("decode token response: %w", err)
	}
	exp, err := expiry(out.Value)
	if err != nil {
		return "", time.Time{}, err
	}
	return out.Value, exp, nil
}

// expiry reads exp from a JWT without checking its signature. The server checks it.
func expiry(jwt string) (time.Time, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("decode JWT claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("JWT has no exp")
	}
	return time.Unix(claims.Exp, 0), nil
}

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Source) backoff(n int) time.Duration {
	if s.Backoff != nil {
		return s.Backoff(n)
	}
	return time.Duration(1<<(n-1)) * time.Second
}

func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
