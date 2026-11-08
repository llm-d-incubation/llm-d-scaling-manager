package scaler

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/credentials"
)

// refusalLogInterval is how often a refused TLS handshake from one remote host
// is logged. KEDA re-dials every few seconds, so without a bound one
// misconfigured ScaledObject fills the log.
const refusalLogInterval = time.Minute

// maxRefusalHosts bounds how many remote hosts the limiter remembers. A
// client that dials from many addresses cannot grow it past this: once full,
// refusals from hosts it does not know are not logged until entries age out.
const maxRefusalHosts = 1024

// refusalLog decides which refused handshakes are logged: at most one per
// remote host per refusalLogInterval. It counts the ones it held back, so the
// next line says how many there were.
type refusalLog struct {
	mu   sync.Mutex
	now  func() time.Time
	last map[string]*refusalEntry
}

type refusalEntry struct {
	at         time.Time
	suppressed int
}

func newRefusalLog(now func() time.Time) *refusalLog {
	if now == nil {
		now = time.Now
	}
	return &refusalLog{now: now, last: map[string]*refusalEntry{}}
}

// admit reports whether a refusal from host is logged now, and how many
// refusals from it were held back since its last logged one.
func (r *refusalLog) admit(host string) (bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	e, ok := r.last[host]
	if ok {
		if now.Sub(e.at) < refusalLogInterval {
			e.suppressed++
			return false, 0
		}
		held := e.suppressed
		e.at, e.suppressed = now, 0
		return true, held
	}
	if len(r.last) >= maxRefusalHosts {
		for h, old := range r.last {
			if now.Sub(old.at) >= refusalLogInterval {
				delete(r.last, h)
			}
		}
		if len(r.last) >= maxRefusalHosts {
			return false, 0
		}
	}
	r.last[host] = &refusalEntry{at: now}
	return true, 0
}

// refusalLoggingCreds wraps the scaler's server TLS credentials and logs each
// refused handshake, rate-limited by refusalLog. Without it a client the
// scaler refuses -- no client certificate, a certificate from another CA or
// for another identity, a plaintext dial -- shows up only on KEDA's side.
type refusalLoggingCreds struct {
	credentials.TransportCredentials
	refusals *refusalLog
	logger   logr.Logger
}

// ServerHandshake runs the wrapped handshake and logs a refusal.
func (c *refusalLoggingCreds) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.TransportCredentials.ServerHandshake(raw)
	if err != nil {
		c.refused(raw.RemoteAddr(), err)
	}
	return conn, info, err
}

// Clone keeps the logging on a copy.
func (c *refusalLoggingCreds) Clone() credentials.TransportCredentials {
	return &refusalLoggingCreds{TransportCredentials: c.TransportCredentials.Clone(), refusals: c.refusals, logger: c.logger}
}

// refused logs one refused handshake from remote. A connection that closed
// before sending anything (a TCP probe) or that the server closed itself is
// not a refused client, and is not logged.
func (c *refusalLoggingCreds) refused(remote net.Addr, err error) {
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return
	}
	addr := ""
	if remote != nil {
		addr = remote.String()
	}
	host := addr
	if h, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
		host = h
	}
	ok, held := c.refusals.admit(host)
	if !ok {
		return
	}
	c.logger.Info("KEDA external scaler refused a TLS handshake; this client reaches no call",
		"remote", addr, "reason", err.Error(), "refusedSinceLastLine", held,
		"hint", "a ScaledObject on this address without the installer's authenticationRef, a plaintext client, or a certificate from another CA or for another identity; at most one line per remote host per minute")
}
