package scalertls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// serveConfig serves cfg on a loopback listener, one byte per accepted
// connection, as the scaler would after a rebind.
func serveConfig(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				if tc, ok := conn.(*tls.Conn); ok && tc.Handshake() == nil {
					_, _ = conn.Write([]byte{1})
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// dialAs completes a handshake as KEDA does with the Secret named client.
func dialAs(t *testing.T, cl client.Reader, client string, addr string) error {
	t.Helper()
	s := getSecret(t, cl, client)
	pair, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(s.Data[corev1.ServiceAccountRootCAKey])
	conn, err := tls.Dial("tcp", addr, &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: roots,
		ServerName: ServiceName + "." + testNS + ".svc", MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = io.ReadFull(conn, make([]byte, 1))
	return err
}

// With the switch on (--external-scaler-tls-live), KEDA's side coming into
// place after start turns the scaler to mutual TLS on the next pass, with no
// restart: SwitchTLS gets a config that verifies KEDA's certificate. Without
// it (the negative control) the pass asks for a restart and switches nothing.
func TestBootstrap_SwitchesToMutualTLSWithNoRestart(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(map[bool]string{true: "live", false: "not live"}[live], func(t *testing.T) {
			var c counter
			cl := newClient(t, &c, interceptor.Funcs{}, placeholders()...)
			st := &kedaStatus{}
			b := newCopyBootstrap(cl, st)
			b.Now = time.Now
			var got *tls.Config
			if live {
				b.AllowedClients = []string{b.ClientSAN}
				b.SwitchTLS = func(cfg *tls.Config) { got = cfg }
			}
			if ca := prepare(t, b); ca != nil {
				t.Fatal("mutual TLS at start with no KEDA copy")
			}
			// The cluster admin's manifest apply creates KEDA's copy now.
			if err := cl.Create(context.Background(), kedaCopyPlaceholder()); err != nil {
				t.Fatal(err)
			}
			b.Check(context.Background())
			state, action := st.get()
			if !live {
				if got != nil || strings.HasPrefix(state, "mutual TLS") || !strings.Contains(action, "restart the controller") {
					t.Fatalf("not live: switched %v, state %q, action %q", got != nil, state, action)
				}
				return
			}
			if got == nil || !strings.HasPrefix(state, "mutual TLS") {
				t.Fatalf("no switch: state %q action %q", state, action)
			}
			if err := dialAs(t, cl, ClientSecretName, serveConfig(t, got)); err != nil {
				t.Fatalf("KEDA cannot connect after the switch: %v", err)
			}
		})
	}
}

// While KEDA's side is in place, a new scaler CA is adopted live: the client
// CA is re-read, and KEDA with the copy of the new client certificate
// connects to the config served since start, with no rebind.
func TestBootstrap_AdoptsANewCALive(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, append(placeholders(), kedaCopyPlaceholder())...)
	st := &kedaStatus{}
	b := newCopyBootstrap(cl, st)
	b.Now = time.Now
	b.AllowedClients = []string{b.ClientSAN}
	b.SwitchTLS = func(*tls.Config) { t.Fatal("switched again while already on mutual TLS") }
	first := prepare(t, b)
	if first == nil {
		t.Fatal("plaintext with KEDA's copy pre-created")
	}
	addr := serveConfig(t, b.MutualTLSConfig())
	if err := dialAs(t, cl, ClientSecretName, addr); err != nil {
		t.Fatal(err)
	}
	// Rotate the CA: empty it, as rotateCommand says.
	s := getSecret(t, cl, CASecretName)
	s.Data = map[string][]byte{corev1.TLSCertKey: {}, corev1.TLSPrivateKeyKey: {}, corev1.ServiceAccountRootCAKey: {}}
	if err := cl.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	b.Check(context.Background())
	b.mu.Lock()
	adopted := Fingerprint(b.clientCA) != Fingerprint(first)
	b.mu.Unlock()
	if !adopted {
		t.Fatal("the new CA was not adopted")
	}
	if err := dialAs(t, cl, ClientSecretName, addr); err != nil {
		t.Fatalf("KEDA with the new client certificate cannot connect to the config served since start: %v", err)
	}
}
