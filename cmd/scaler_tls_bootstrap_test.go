package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
)

// observed reports a handshake only after verify accepted it: a refused
// client must never count as KEDA presenting its certificate.
func TestObservedOnlyAfterVerifyAccepts(t *testing.T) {
	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("leaf")}}}
	calls := 0
	observe := func(tls.ConnectionState) { calls++ }

	refuse := observed(func(tls.ConnectionState) error { return errors.New("not KEDA") }, observe)
	if err := refuse(cs); err == nil || calls != 0 {
		t.Fatalf("a refused handshake: err %v, observed %d times", err, calls)
	}
	accept := observed(func(tls.ConnectionState) error { return nil }, observe)
	if err := accept(cs); err != nil || calls != 1 {
		t.Fatalf("an accepted handshake: err %v, observed %d times", err, calls)
	}
	none := observed(nil, observe)
	if err := none(cs); err != nil || calls != 2 {
		t.Fatalf("no verify: err %v, observed %d times", err, calls)
	}
}
