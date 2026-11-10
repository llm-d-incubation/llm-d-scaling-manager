package scaler

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// errTLSBadCert is the handshake error the scaler logs when KEDA, holding the CA
// of an earlier install, rejects the scaler's certificate.
var errTLSBadCert = errors.New("remote error: tls: bad certificate")

// The failure this exists for (a reinstall into a namespace KEDA already
// dialled): KEDA keeps its pooled connection, every handshake is refused, and
// no call is ever served. After reachConfirm of that, and until something is
// served, the scaler reports itself unreachable.
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
	for i := 0; i < 4; i++ {
		c.t = c.t.Add(20 * time.Second)
		r.refusedHandshake(errTLSBadCert.Error())
	}
	u, detail := r.state()
	if !u {
		t.Fatalf("not unreachable after %s of refusals with nothing served", 80*time.Second)
	}
	for _, want := range []string{"refused 5 TLS handshake", "bad certificate", "rollout restart deployment/keda-operator"} {
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
	r.refusedHandshake(errTLSBadCert.Error())
	c.t = c.t.Add(2 * time.Minute)
	r.refusedHandshake(errTLSBadCert.Error())
	if u, _ := r.state(); !u {
		t.Fatal("not unreachable: refusals for 2m after the last call, which was more than the window ago")
	}
	// The refusals stop (KEDA restarted and reconnected elsewhere, or its
	// ScaledObjects are gone): once the last one is older than the window,
	// the condition clears.
	c.t = c.t.Add(reachWindow + time.Second)
	if u, _ := r.state(); u {
		t.Fatal("still unreachable with no refusal in the window")
	}
}

// An open stream (StreamIsActive) is a client that reaches the scaler however
// long ago it opened.
func TestReachabilityOpenStream(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newReachability(c.now)
	end := r.begin()
	c.t = c.t.Add(reachWindow + time.Minute)
	r.refusedHandshake(errTLSBadCert.Error())
	c.t = c.t.Add(2 * time.Minute)
	r.refusedHandshake(errTLSBadCert.Error())
	if u, _ := r.state(); u {
		t.Fatal("unreachable while a stream is open")
	}
	end()
	c.t = c.t.Add(reachWindow + time.Second)
	r.refusedHandshake(errTLSBadCert.Error())
	c.t = c.t.Add(2 * time.Minute)
	r.refusedHandshake(errTLSBadCert.Error())
	if u, _ := r.state(); !u {
		t.Fatal("not unreachable once the stream closed and only refusals followed")
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
	creds.refused(remote, errTLSBadCert)
	c.t = c.t.Add(reachConfirm)
	creds.refused(remote, errTLSBadCert)
	if u, _ := r.state(); !u {
		t.Fatalf("refusals through the credentials did not reach the tracker (refused=%d)", r.refused)
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
	r.refusedHandshake(errTLSBadCert.Error())
	c.t = c.t.Add(reachConfirm)
	r.refusedHandshake(errTLSBadCert.Error())
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
