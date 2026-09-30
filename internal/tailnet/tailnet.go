// Package tailnet joins the tailnet as an ephemeral tsnet node in the background.
package tailnet

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	_ "tailscale.com/feature/identityfederation"
	"tailscale.com/tsnet"
)

// Tag is the ACL tag the federated identity grants to gauger nodes.
const Tag = "tag:gauger-ci"

// Config is the node's settings. ClientID and Audience are the infra outputs
// gauger_ci_client_id and gauger_ci_audience.
type Config struct {
	Dir      string
	Hostname string
	ClientID string
	Audience string
	// UpTimeout bounds one join attempt.
	UpTimeout time.Duration
	Log       *log.Logger
	// Verbose sends tsnet's own logs to Log.
	Verbose bool
}

// Node joins in the background. Wait returns once it has joined.
type Node struct {
	cfg  Config
	done chan struct{}

	mu     sync.Mutex
	srv    *tsnet.Server
	err    error
	joined time.Duration
}

// Join starts joining and returns at once. It retries with backoff until
// ctx is cancelled.
func Join(ctx context.Context, cfg Config) *Node {
	n := &Node{cfg: cfg, done: make(chan struct{})}
	go n.run(ctx)
	return n
}

func (n *Node) run(ctx context.Context) {
	defer close(n.done)
	start := time.Now()
	backoff := 2 * time.Second
	for attempt := 1; ; attempt++ {
		srv := n.server()
		upCtx, cancel := context.WithTimeout(ctx, n.cfg.UpTimeout)
		_, err := srv.Up(upCtx)
		cancel()
		if err == nil {
			n.mu.Lock()
			n.srv, n.err, n.joined = srv, nil, time.Since(start)
			n.mu.Unlock()
			n.cfg.Log.Printf("joined the tailnet as %s in %s", n.cfg.Hostname, time.Since(start).Round(time.Millisecond))
			return
		}
		srv.Close()
		n.mu.Lock()
		n.err = err
		n.mu.Unlock()
		n.cfg.Log.Printf("join attempt %d: %v", attempt, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

func (n *Node) server() *tsnet.Server {
	logf := func(string, ...any) {}
	if n.cfg.Verbose {
		logf = n.cfg.Log.Printf
	}
	return &tsnet.Server{
		Dir:           n.cfg.Dir,
		Hostname:      n.cfg.Hostname,
		Ephemeral:     true,
		AdvertiseTags: []string{Tag},
		ClientID:      n.cfg.ClientID,
		Audience:      n.cfg.Audience,
		Logf:          logf,
		UserLogf:      n.cfg.Log.Printf,
	}
}

// Wait blocks until the node has joined or ctx ends, and returns an HTTP
// client that dials through the tailnet.
func (n *Node) Wait(ctx context.Context) (*http.Client, error) {
	select {
	case <-ctx.Done():
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.err != nil {
			return nil, fmt.Errorf("join the tailnet: %w", n.err)
		}
		return nil, ctx.Err()
	case <-n.done:
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.srv == nil {
		if n.err != nil {
			return nil, fmt.Errorf("join the tailnet: %w", n.err)
		}
		return nil, errors.New("join the tailnet: stopped before joining")
	}
	return n.srv.HTTPClient(), nil
}

// JoinTime is how long the join took, or zero if it has not joined.
func (n *Node) JoinTime() time.Duration {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.joined
}

// Close logs out, which removes the ephemeral node at once instead of after
// its idle timeout, then shuts tsnet down and deletes its state. Cancel the
// context given to Join first, so that no join attempt is still running.
func (n *Node) Close(ctx context.Context) error {
	select {
	case <-n.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	n.mu.Lock()
	srv := n.srv
	n.mu.Unlock()
	if srv == nil {
		return nil
	}
	var errs []error
	if lc, err := srv.LocalClient(); err != nil {
		errs = append(errs, err)
	} else if err := lc.Logout(ctx); err != nil {
		errs = append(errs, fmt.Errorf("log out: %w", err))
	}
	if err := srv.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(n.cfg.Dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
