// Package tailnet joins the tailnet as an ephemeral tsnet node in the background.
package tailnet

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"tailscale.com/envknob"
	_ "tailscale.com/feature/identityfederation"
	"tailscale.com/tsnet"
)

// Tag is the ACL tag the federated identity grants to gauger nodes.
const Tag = "tag:gauger-ci"

// KeyAttributes is appended to a client ID without its own, so the minted
// auth key makes an ephemeral, preauthorized node.
const KeyAttributes = "?ephemeral=true&preauthorized=true"

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
	cfg      Config
	done     chan struct{}
	noRetry  chan struct{}
	stopOnce sync.Once

	mu     sync.Mutex
	srv    *tsnet.Server
	err    error
	joined time.Duration
}

// Join starts joining and returns at once. It retries with backoff until
// ctx is cancelled or StopRetrying is called.
func Join(ctx context.Context, cfg Config) *Node {
	n := &Node{cfg: cfg, done: make(chan struct{}), noRetry: make(chan struct{})}
	go n.run(ctx)
	return n
}

// StopRetrying lets the attempt in progress finish but starts no new one, so
// that a node that keeps failing does not hold up the post step.
func (n *Node) StopRetrying() {
	n.stopOnce.Do(func() { close(n.noRetry) })
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
		case <-n.noRetry:
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
	clientID := n.cfg.ClientID
	if clientID != "" && !strings.Contains(clientID, "?") {
		clientID += KeyAttributes
	}
	envknob.SetNoLogsNoSupport()
	return &tsnet.Server{
		Dir:           n.cfg.Dir,
		Hostname:      n.cfg.Hostname,
		Ephemeral:     true,
		AdvertiseTags: []string{Tag},
		ClientID:      clientID,
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

// Qualify rewrites a server URL whose host is a bare name, such as
// http://gauger-server:4318, to the node's full MagicDNS name. tsnet only
// resolves full names itself and sends a bare one to the system resolver,
// which on a runner does not know the tailnet.
func (n *Node) Qualify(ctx context.Context, rawURL string) (string, error) {
	n.mu.Lock()
	srv := n.srv
	n.mu.Unlock()
	if srv == nil {
		return "", errors.New("not joined")
	}
	lc, err := srv.LocalClient()
	if err != nil {
		return "", err
	}
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return "", fmt.Errorf("read the MagicDNS suffix: %w", err)
	}
	if st.CurrentTailnet == nil || st.CurrentTailnet.MagicDNSSuffix == "" {
		return "", errors.New("the tailnet has no MagicDNS suffix")
	}
	return QualifyURL(rawURL, st.CurrentTailnet.MagicDNSSuffix)
}

// QualifyURL appends suffix to the host of rawURL when the host is a bare
// name. URLs with a dotted host or an IP address come back unchanged.
func QualifyURL(rawURL, suffix string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host == "" || strings.Contains(host, ".") || strings.Contains(host, ":") {
		return rawURL, nil
	}
	host += "." + strings.Trim(suffix, ".")
	if port := u.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	}
	u.Host = host
	return u.String(), nil
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
