package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func jwt(exp time.Time) string {
	payload, _ := json.Marshal(map[string]int64{"exp": exp.Unix(), "iat": exp.Add(-5 * time.Minute).Unix()})
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

type fakeIssuer struct {
	calls atomic.Int32
	fail  atomic.Int32
	exp   func() time.Time
}

func (f *fakeIssuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := f.calls.Add(1)
	if r.Header.Get("Authorization") != "bearer request-token" || r.URL.Query().Get("audience") != "gauger-server" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if n <= f.fail.Load() {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, `{"value":%q}`, jwt(f.exp()))
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newSource(t *testing.T, issuer *fakeIssuer, c *clock) *Source {
	srv := httptest.NewServer(issuer)
	t.Cleanup(srv.Close)
	return &Source{
		RequestURL:   srv.URL + "/token?api-version=2.0",
		RequestToken: "request-token",
		Audience:     "gauger-server",
		Now:          c.now,
		Backoff:      func(int) time.Duration { return time.Millisecond },
	}
}

func TestTokenIsReusedUntilAMinuteBeforeExp(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	issuer := &fakeIssuer{exp: func() time.Time { return c.t.Add(5 * time.Minute) }}
	s := newSource(t, issuer, c)

	first, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(3*time.Minute + 59*time.Second)
	if again, _ := s.Token(context.Background()); again != first || issuer.calls.Load() != 1 {
		t.Fatalf("token was refetched %d times before the refresh point", issuer.calls.Load()-1)
	}
	c.t = c.t.Add(time.Second)
	fresh, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fresh == first || issuer.calls.Load() != 2 {
		t.Fatal("token was not refreshed 60 s before exp")
	}
}

func TestFetchIsRetriedThreeTimes(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	issuer := &fakeIssuer{exp: func() time.Time { return c.t.Add(5 * time.Minute) }}
	issuer.fail.Store(3)
	s := newSource(t, issuer, c)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("fourth attempt should succeed: %v", err)
	}

	issuer.calls.Store(0)
	issuer.fail.Store(100)
	s = newSource(t, issuer, c)
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("Token succeeded with every fetch failing")
	}
	if got := issuer.calls.Load(); got != 4 {
		t.Fatalf("made %d attempts, want 1 plus 3 retries", got)
	}
}

func TestFailedRefreshFallsBackToTheUnexpiredToken(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	issuer := &fakeIssuer{exp: func() time.Time { return c.t.Add(5 * time.Minute) }}
	s := newSource(t, issuer, c)
	first, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	issuer.fail.Store(100)
	c.t = c.t.Add(4*time.Minute + 30*time.Second)
	if got, err := s.Token(context.Background()); err != nil || got != first {
		t.Fatalf("Token = %q, %v; want the cached token while it is still valid", got, err)
	}
	c.t = c.t.Add(time.Minute)
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("Token returned an expired token")
	}
}

func TestFromEnvNeedsIDTokenPermission(t *testing.T) {
	if _, err := FromEnv(func(string) string { return "" }, "gauger-server"); err == nil {
		t.Fatal("FromEnv succeeded without ACTIONS_ID_TOKEN_REQUEST_URL")
	}
}
