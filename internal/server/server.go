// Package server serves the MCP handler over a peer-authenticated Unix socket
// (production) or stdio (development and tests only).
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/audit"
	"github.com/mhmdxsadk/latch/internal/peercred"
)

const (
	// SocketMode is fixed. Access is further restricted by peer credentials.
	SocketMode os.FileMode = 0o660

	// Endpoint is the only HTTP path served.
	Endpoint = "/mcp"

	maxBodyBytes = 64 << 10
)

// Options configure Listen.
type Options struct {
	Path  string
	Group string // optional group name that should own the socket
}

// Listen creates the Unix socket with mode 0660 and the requested group. It
// refuses to replace anything that is not a stale socket.
func Listen(o Options) (net.Listener, error) {
	if fi, err := os.Lstat(o.Path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", o.Path)
		}
		if c, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "unix", o.Path); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s is already in use by another process", o.Path)
		}
		if err := os.Remove(o.Path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	gid := -1
	if o.Group != "" {
		g, err := GroupID(o.Group)
		if err != nil {
			return nil, err
		}
		gid = int(g)
	}

	// Create the socket with no permissions for "other" from the start so there
	// is no window in which it is more open than intended.
	old := syscall.Umask(0o117)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", o.Path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Listener, error) {
		ln.Close()
		os.Remove(o.Path)
		return nil, err
	}
	if gid >= 0 {
		if err := os.Chown(o.Path, -1, gid); err != nil {
			return fail(fmt.Errorf("chown socket: %w", err))
		}
	}
	if err := os.Chmod(o.Path, SocketMode); err != nil {
		return fail(fmt.Errorf("chmod socket: %w", err))
	}
	return ln, nil
}

// GroupID resolves the socket group to its numeric ID. Root's group is refused:
// it would let any root process through the peer check, which defeats the
// point of running the agent unprivileged.
func GroupID(name string) (uint32, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, fmt.Errorf("lookup socket group: %w", err)
	}
	gid, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid gid for group %q", name)
	}
	if gid == 0 {
		return 0, fmt.Errorf("socket group %q is gid 0; use a dedicated group", name)
	}
	return uint32(gid), nil
}

// credListener rejects peers whose kernel-verified credentials are not in the
// socket group. Failing to read credentials is a rejection.
type credListener struct {
	net.Listener
	gid   uint32
	audit *audit.Logger
}

type credConn struct {
	net.Conn
	peer audit.Peer
}

func (l *credListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		cred, err := peercred.FromConn(c)
		if err != nil {
			l.audit.Connection(context.Background(), audit.Peer{}, audit.OutcomeRejected, "no_credentials")
			c.Close()
			continue
		}
		p := audit.Peer{UID: cred.UID, PID: cred.PID}
		if len(cred.GIDs) > 0 {
			p.GID = cred.GIDs[0]
		}
		if !peercred.InGroup(cred, l.gid) {
			l.audit.Connection(context.Background(), p, audit.OutcomeRejected, "peer_not_allowed")
			c.Close()
			continue
		}
		return &credConn{Conn: c, peer: p}, nil
	}
}

// ServeUnix serves handler on ln until ctx is cancelled. Only peers in group
// gid are accepted.
func ServeUnix(ctx context.Context, ln net.Listener, handler http.Handler, gid uint32, al *audit.Logger) error {
	mux := http.NewServeMux()
	mux.Handle(Endpoint, limitBody(handler))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if cc, ok := c.(*credConn); ok {
				return context.WithValue(ctx, peerKey{}, cc.peer)
			}
			return ctx
		},
	}

	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(sctx)
		})
	}
	go func() { <-ctx.Done(); shutdown() }()

	err := srv.Serve(&credListener{Listener: ln, gid: gid, audit: al})
	shutdown()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type peerKey struct{}

// PeerFromContext returns the authenticated peer of the request, if any.
func PeerFromContext(ctx context.Context) (audit.Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(audit.Peer)
	return p, ok
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// Handler adapts an MCP server to a stateless Streamable HTTP handler.
// Stateless mode keeps no per-client session state, which suits a read-only
// request/response API and removes session-hijack surface.
func Handler(s *sdk.Server) http.Handler {
	return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

// RunStdio serves a single client over stdin/stdout. The peer is the parent
// process and is trusted by construction, so this is for development and
// testing only; it provides no privilege separation.
func RunStdio(ctx context.Context, s *sdk.Server) error {
	return s.Run(ctx, &sdk.StdioTransport{})
}
