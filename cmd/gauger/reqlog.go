package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync"
	"time"
)

// reqLog writes one JSON line per request for the Funnel load test.
type reqLog struct {
	base http.RoundTripper
	mu   sync.Mutex
	f    *os.File
	enc  *json.Encoder
}

type reqRecord struct {
	Start       time.Time `json:"start"`
	Path        string    `json:"path"`
	Bytes       int64     `json:"bytes"`
	HeaderBytes int       `json:"header_bytes"`
	Status      int       `json:"status"`
	Proto       string    `json:"proto,omitempty"`
	LatencyMS   float64   `json:"latency_ms"`
	GotConn     bool      `json:"got_conn"`
	Reused      bool      `json:"reused"`
	WasIdle     bool      `json:"was_idle"`
	IdleMS      float64   `json:"idle_ms,omitempty"`
	DNSMS       float64   `json:"dns_ms,omitempty"`
	ConnectMS   float64   `json:"connect_ms,omitempty"`
	TLSMS       float64   `json:"tls_ms,omitempty"`
	TTFBMS      float64   `json:"ttfb_ms,omitempty"`
	ErrKind     string    `json:"err_kind,omitempty"`
	Err         string    `json:"err,omitempty"`
}

func openReqLog(path string) (*reqLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &reqLog{base: http.DefaultTransport, f: f, enc: json.NewEncoder(f)}, nil
}

func (l *reqLog) Close() error { return l.f.Close() }

func (l *reqLog) RoundTrip(req *http.Request) (*http.Response, error) {
	var (
		mu                         sync.Mutex
		rec                        reqRecord
		dnsAt, connectAt, tlsAt    time.Time
		dnsErr, connectErr, tlsErr error
		connectOK                  bool
	)
	start := time.Now()
	rec.Start = start.UTC()
	rec.Path = req.URL.Path
	rec.Bytes = req.ContentLength
	for k, vs := range req.Header {
		for _, v := range vs {
			rec.HeaderBytes += len(k) + len(v) + 4
		}
	}
	since := func(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { mu.Lock(); dnsAt = time.Now(); mu.Unlock() },
		DNSDone: func(i httptrace.DNSDoneInfo) {
			mu.Lock()
			rec.DNSMS, dnsErr = since(dnsAt), i.Err
			mu.Unlock()
		},
		ConnectStart: func(string, string) { mu.Lock(); connectAt = time.Now(); mu.Unlock() },
		ConnectDone: func(_, _ string, err error) {
			mu.Lock()
			if err == nil {
				connectOK, rec.ConnectMS = true, since(connectAt)
			} else if !connectOK {
				connectErr = err
			}
			mu.Unlock()
		},
		TLSHandshakeStart: func() { mu.Lock(); tlsAt = time.Now(); mu.Unlock() },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			mu.Lock()
			rec.TLSMS, tlsErr = since(tlsAt), err
			mu.Unlock()
		},
		GotConn: func(i httptrace.GotConnInfo) {
			mu.Lock()
			rec.GotConn, rec.Reused, rec.WasIdle = true, i.Reused, i.WasIdle
			rec.IdleMS = float64(i.IdleTime.Microseconds()) / 1000
			mu.Unlock()
		},
		GotFirstResponseByte: func() { mu.Lock(); rec.TTFBMS = since(start); mu.Unlock() },
	}
	resp, err := l.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))

	mu.Lock()
	rec.LatencyMS = since(start)
	if err != nil {
		rec.Err = err.Error()
		var ne net.Error
		switch {
		case tlsErr != nil:
			rec.ErrKind = "tls"
		case dnsErr != nil:
			rec.ErrKind = "dns"
		case connectErr != nil && !connectOK:
			rec.ErrKind = "connect"
		case errors.Is(err, context.Canceled):
			rec.ErrKind = "canceled"
		case !rec.GotConn:
			rec.ErrKind = "connect"
		case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
			rec.ErrKind = "timeout"
		default:
			rec.ErrKind = "other"
		}
	} else {
		rec.Status, rec.Proto = resp.StatusCode, resp.Proto
	}
	l.mu.Lock()
	l.enc.Encode(rec)
	l.mu.Unlock()
	mu.Unlock()
	return resp, err
}
