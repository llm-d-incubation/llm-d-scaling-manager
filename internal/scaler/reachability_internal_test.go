package scaler

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/credentials/insecure"
)

// errTLSBadCert is the handshake error the scaler logs when KEDA, holding the CA
// of an earlier install, rejects the scaler's certificate.
var errTLSBadCert = errors.New("remote error: tls: bad certificate")

// refuseEvery records n refusals, d apart, starting now.
func refuseEvery(c *clock, r *reachability, n int, d time.Duration) {
	for i := 0; i < n; i++ {
		if i > 0 {
			c.t = c.t.Add(d)
		}
		r.refusedHandshake(errTLSBadCert.Error())
	}
}

// The failure this exists for (the scaler's CA changed while KEDA's operator
// kept running): KEDA keeps its pooled connection, every handshake is
// refused, and no call is ever served. Once enough refusals span reachConfirm,
// and until something is served, the scaler reports itself unreachable.
func TestReachabilityEveryHandshakeRefused(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newReachability(c.now)

	if u, _ := r.state(); u {
		t.Fatal("unreachable before anything happened")
	}
	r.refusedHandshake(errTLSBadCert.Error())
	if u, _ := r.state(); u {
		t.Fatal("unreachable on the first refusal: one refusal is not a run of them")
	}
	refuseEvery(c, r, 4, 20*time.Second)
	u, detail := r.state()
	if !u {
		t.Fatalf("not unreachable after %s of refusals with nothing served", 60*time.Second)
	}
	for _, want := range []string{"refused 5 TLS handshake", "bad certificate", "rollout restart deployment/keda-operator", "first record does not look like a TLS handshake"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}

	// One call served ends it at once.
	r.served()
	if u, _ := r.state(); u {
		t.Fatal("still unreachable after a call was served")
	}
}

// One stray connection (a port scan, a prober that dials once) followed by
// silence is not "no client reaches the scaler": it must not page.
func TestReachabilityOneRefusalThenSilence(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newReachability(c.now)
	r.refusedHandshake("tls: first record does not look like a TLS handshake")
	for _, after := range []time.Duration{61 * time.Second, 2 * time.Minute, reachWindow} {
		c.t = time.Unix(1_000_000, 0).Add(after)
		if u, _ := r.state(); u {
			t.Fatalf("unreachable %s after a single refusal", after)
		}
	}
	// Nor a burst of re-dials inside one second.
	refuseEvery(c, r, 5, 100*time.Millisecond)
	c.t = c.t.Add(2 * time.Minute)
	if u, _ := r.state(); u {
		t.Fatal("unreachable on a burst of refusals within a second")
	}
	// A prober that dials once every 4 minutes: never three in the window.
	c = &clock{t: time.Unix(2_000_000, 0)}
	r = newReachability(c.now)
	for i := 0; i < 6; i++ {
		r.refusedHandshake("tls: first record does not look like a TLS handshake")
		if u, _ := r.state(); u {
			t.Fatalf("unreachable on a prober's refusal %d, 4 minutes apart", i+1)
		}
		c.t = c.t.Add(4 * time.Minute)
	}
}

// A KEDA with a stale connection re-dials with gRPC's backoff, which grows to
// its 120 s cap. Within a few minutes that is unreachable.
func TestReachabilityStaleKEDABackoff(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newReachability(c.now)
	var tripped time.Duration
	start := c.t
	for _, wait := range []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 64 * time.Second, 120 * time.Second, 120 * time.Second, 120 * time.Second} {
		c.t = c.t.Add(wait)
		r.refusedHandshake(errTLSBadCert.Error())
		if u, _ := r.state(); u && tripped == 0 {
			tripped = c.t.Sub(start)
		}
	}
	if tripped == 0 || tripped > 3*time.Minute {
		t.Fatalf("a KEDA re-dialling with backoff tripped after %s, want within 3m", tripped)
	}
}

// A KEDA that works keeps one connection and calls on it; a stray client
// (a plaintext probe, a ScaledObject without the authenticationRef) refused
// meanwhile is not "no client reaches the scaler".
func TestReachabilityRefusalsBesideServedCalls(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newReachability(c.now)
	for i := 0; i < 20; i++ {
		r.served()
		c.t = c.t.Add(15 * time.Second)
		r.refusedHandshake("tls: first record does not look like a TLS handshake")
	}
	if u, _ := r.state(); u {
		t.Fatal("unreachable while calls are served every 15s")
	}
}

// A served call older than reachWindow does not hold the condition off, and
// refusals older than reachWindow do not raise it.
func TestReachabilityWindow(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newReachability(c.now)
	r.served()
	c.t = c.t.Add(reachWindow)
	refuseEvery(c, r, 3, time.Minute)
	if u, _ := r.state(); !u {
		t.Fatal("not unreachable: refusals for 2m after the last call, which was more than the window ago")
	}
	// The refusals stop (KEDA restarted and reconnected elsewhere, or its
	// ScaledObjects are gone): once they are older than the window, the
	// condition clears.
	c.t = c.t.Add(reachWindow + time.Second)
	if u, _ := r.state(); u {
		t.Fatal("still unreachable with no refusal in the window")
	}
}

// An open stream (StreamIsActive) is a client that reaches the scaler however
// long ago it opened, and its end counts as served.
func TestReachabilityOpenStream(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newReachability(c.now)
	end := r.begin()
	c.t = c.t.Add(reachWindow + time.Minute)
	refuseEvery(c, r, 3, time.Minute)
	if u, _ := r.state(); u {
		t.Fatal("unreachable while a stream is open")
	}
	end()
	if u, _ := r.state(); u {
		t.Fatal("unreachable right after a stream closed: its end is a served call")
	}
	c.t = c.t.Add(reachWindow + time.Second)
	refuseEvery(c, r, 3, time.Minute)
	if u, _ := r.state(); !u {
		t.Fatal("not unreachable once the stream closed long ago and only refusals followed")
	}
}

// The refused-handshake path feeds the tracker, a completed handshake clears
// it, and a connection closed before it sent anything is not a refusal.
func TestRefusalLoggingCredsFeedReachability(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newReachability(c.now)
	creds := &refusalLoggingCreds{refusals: newRefusalLog(c.now), reach: r, logger: logr.Discard()}
	remote := &net.TCPAddr{IP: net.ParseIP("10.0.2.77"), Port: 40842}

	creds.refused(remote, net.ErrClosed)
	if r.refused != 0 {
		t.Fatalf("a closed connection counted as %d refusal(s)", r.refused)
	}
	for i := 0; i < 3; i++ {
		creds.refused(remote, errTLSBadCert)
		c.t = c.t.Add(reachConfirm / 2)
	}
	if u, _ := r.state(); !u {
		t.Fatalf("refusals through the credentials did not reach the tracker (refused=%d)", r.refused)
	}
}

// A clone of the credentials (gRPC may clone them per connection) reports to
// the same tracker.
func TestRefusalLoggingCredsCloneKeepsReach(t *testing.T) {
	r := newReachability(nil)
	creds := &refusalLoggingCreds{TransportCredentials: insecure.NewCredentials(), refusals: newRefusalLog(nil), reach: r, logger: logr.Discard()}
	clone, ok := creds.Clone().(*refusalLoggingCreds)
	if !ok || clone.reach != r {
		t.Fatal("Clone dropped the reachability tracker: a cloned connection's refusals would not count")
	}
}

// The reporter publishes a change once, in both directions, and not again
// while the state holds: the controller-status ConfigMap is rewritten only
// when the condition flips.
func TestReachReporterPublishesChanges(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newReachability(c.now)
	type call struct {
		unreachable bool
		detail      string
	}
	var calls []call
	p := &reachReporter{reach: r, logger: logr.Discard(), hook: func(u bool, d string) { calls = append(calls, call{u, d}) }}

	p.check()
	p.check()
	if len(calls) != 1 || calls[0].unreachable {
		t.Fatalf("start: hook calls %+v, want one reachable", calls)
	}
	refuseEvery(c, r, 2, reachConfirm)
	p.check()
	c.t = c.t.Add(10 * time.Second)
	r.refusedHandshake(errTLSBadCert.Error())
	p.check()
	if len(calls) != 2 || !calls[1].unreachable || !strings.Contains(calls[1].detail, "bad certificate") {
		t.Fatalf("refused: hook calls %+v, want a second, unreachable, naming the reason", calls)
	}
	r.served()
	p.check()
	if len(calls) != 3 || calls[2].unreachable || calls[2].detail != "" {
		t.Fatalf("served: hook calls %+v, want a third, reachable, with no detail", calls)
	}
}
