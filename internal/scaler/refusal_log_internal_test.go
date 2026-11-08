package scaler

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
)

// clock is a settable time source for refusalLog.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestRefusalLogOncePerHostPerInterval(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newRefusalLog(c.now)

	if ok, held := r.admit("10.0.0.1"); !ok || held != 0 {
		t.Fatalf("first refusal from a host: admit = %v, %d; want true, 0", ok, held)
	}
	for i := 0; i < 3; i++ {
		c.t = c.t.Add(10 * time.Second)
		if ok, _ := r.admit("10.0.0.1"); ok {
			t.Fatalf("refusal %d within the interval was logged", i+2)
		}
	}
	if ok, _ := r.admit("10.0.0.2"); !ok {
		t.Fatal("another host was held back by the first host's line")
	}
	c.t = c.t.Add(refusalLogInterval)
	ok, held := r.admit("10.0.0.1")
	if !ok || held != 3 {
		t.Fatalf("after the interval: admit = %v, %d; want true and the 3 held back", ok, held)
	}
	if ok, _ := r.admit("10.0.0.1"); ok {
		t.Fatal("the interval did not restart at the logged line")
	}
}

func TestRefusalLogBoundsHosts(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newRefusalLog(c.now)
	for i := 0; i < maxRefusalHosts; i++ {
		if ok, _ := r.admit(fmt.Sprintf("h%d", i)); !ok {
			t.Fatalf("host %d below the bound was not logged", i)
		}
	}
	if ok, _ := r.admit("one-too-many"); ok {
		t.Fatal("a host past the bound was logged while every entry is fresh")
	}
	if len(r.last) != maxRefusalHosts {
		t.Fatalf("remembered %d hosts, want the bound %d", len(r.last), maxRefusalHosts)
	}
	c.t = c.t.Add(refusalLogInterval)
	if ok, _ := r.admit("one-too-many"); !ok {
		t.Fatal("aged-out entries were not dropped to make room")
	}
	if len(r.last) != 1 {
		t.Fatalf("remembered %d hosts after pruning, want 1", len(r.last))
	}
}

func TestRefusedNamesRemoteAndReasonAndSkipsClosedConnections(t *testing.T) {
	var lines []string
	lg := funcr.NewJSON(func(obj string) { lines = append(lines, obj) }, funcr.Options{})
	creds := &refusalLoggingCreds{refusals: newRefusalLog(nil), logger: lg}
	remote := &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 41234}

	creds.refused(remote, io.EOF)
	creds.refused(remote, fmt.Errorf("read: %w", net.ErrClosed))
	if len(lines) != 0 {
		t.Fatalf("a connection closed before or during the handshake was logged as a refusal: %v", lines)
	}

	creds.refused(remote, errors.New("tls: client didn't provide a certificate"))
	creds.refused(&net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 41235}, errors.New("tls: client didn't provide a certificate"))
	if len(lines) != 1 {
		t.Fatalf("got %d lines for two refusals from one host within a minute, want 1: %v", len(lines), lines)
	}
	for _, want := range []string{"refused a TLS handshake", `"remote":"10.1.2.3:41234"`, "didn't provide a certificate"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("line lacks %q: %s", want, lines[0])
		}
	}
}
