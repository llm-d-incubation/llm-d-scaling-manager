package scaler

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// NewTLSConfig builds the server-side TLS configuration for the external
// scaler.
//
// getCertificate serves the scaler's own certificate; pass a
// certwatcher.CertWatcher's GetCertificate so a rotated Secret is picked up
// without a restart. clientCAFile, when non-empty, turns on mutual TLS: every
// client must present a certificate that chains to a CA in that PEM bundle,
// and one that does not is refused during the handshake, before any RPC -- and
// so before any call can register a workload or read a decision. KEDA presents
// one when the trigger's TriggerAuthentication carries tlsClientCert and
// tlsClientKey. The bundle is read once; rotating it needs a restart.
//
// allowedClients, when non-empty, narrows mutual TLS from "any certificate the
// CA signed" to the named identities: the client's leaf certificate must carry
// one of them as a DNS SAN, a URI SAN, or its subject Common Name (exact
// match). It needs clientCAFile, since without a verified chain a presented
// name proves nothing. Empty accepts any certificate the CA signed.
func NewTLSConfig(getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error), clientCAFile string, allowedClients []string) (*tls.Config, error) {
	if getCertificate == nil {
		return nil, errors.New("external scaler TLS needs a serving certificate")
	}
	if len(allowedClients) > 0 && clientCAFile == "" {
		return nil, errors.New("an external scaler client identity allowlist needs a client CA: without mutual TLS no client name is verified")
	}
	cfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: getCertificate,
	}
	if clientCAFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("reading external scaler client CA %q: %w", clientCAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("external scaler client CA %q holds no PEM certificate", clientCAFile)
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	if len(allowedClients) > 0 {
		allowed := slices.Clone(allowedClients)
		// VerifyConnection, not VerifyPeerCertificate: it runs on every
		// handshake, resumed sessions included, and after the chain has been
		// verified against ClientCAs.
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			return verifyClientIdentity(cs.PeerCertificates, allowed)
		}
	}
	return cfg, nil
}

// TLSFlags are the external scaler's TLS flags as given on the command line.
// ClientCASet and ClientSANSet say whether --external-scaler-client-ca and
// --external-scaler-client-san were passed at all, empty or not.
type TLSFlags struct {
	CertPath     string
	ClientCA     string
	ClientCASet  bool
	ClientSAN    string
	ClientSANSet bool
}

// CheckTLSFlags validates the external scaler's TLS flags and returns the
// client identities to accept. A flag passed with a value that means nothing
// is an error, not the default: an empty --external-scaler-client-ca next to a
// serving certificate would serve TLS without mutual TLS, and a
// --external-scaler-client-san of only commas and spaces would accept any
// certificate the client CA signed. Both read as a pinned client and are not.
func CheckTLSFlags(f TLSFlags) ([]string, error) {
	if f.ClientCA != "" && f.CertPath == "" {
		return nil, errors.New("--external-scaler-client-ca needs --external-scaler-cert-path: mutual TLS needs a serving certificate")
	}
	if f.ClientCASet && strings.TrimSpace(f.ClientCA) == "" && f.CertPath != "" {
		return nil, errors.New("--external-scaler-client-ca was passed empty: give the client CA bundle, or drop the flag to serve TLS without mutual TLS")
	}
	allowed := ParseClientIdentities(f.ClientSAN)
	if f.ClientSANSet && len(allowed) == 0 {
		return nil, fmt.Errorf("--external-scaler-client-san=%q names no client identity: give one or more, comma-separated, or drop the flag", f.ClientSAN)
	}
	if len(allowed) > 0 && f.ClientCA == "" {
		return nil, errors.New("--external-scaler-client-san needs --external-scaler-client-ca: a client name is verified only under mutual TLS")
	}
	return allowed, nil
}

// ParseClientIdentities splits a comma-separated identity list, trimming
// spaces and dropping empty entries.
func ParseClientIdentities(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// verifyClientIdentity accepts a client whose leaf certificate names one of
// allowed as a DNS SAN, a URI SAN, or its subject Common Name.
func verifyClientIdentity(peers []*x509.Certificate, allowed []string) error {
	if len(peers) == 0 {
		return errors.New("external scaler: client presented no certificate")
	}
	leaf := peers[0]
	names := make([]string, 0, len(leaf.DNSNames)+len(leaf.URIs)+1)
	names = append(names, leaf.DNSNames...)
	for _, u := range leaf.URIs {
		names = append(names, u.String())
	}
	if leaf.Subject.CommonName != "" {
		names = append(names, leaf.Subject.CommonName)
	}
	for _, n := range names {
		if slices.Contains(allowed, n) {
			return nil
		}
	}
	return fmt.Errorf("external scaler: client certificate identities %q are not in the allowed client list", names)
}
