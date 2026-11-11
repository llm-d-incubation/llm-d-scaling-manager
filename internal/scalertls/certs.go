package scalertls

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // the RFC 5280 key identifier, see subjectKeyID
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"
)

// The shape of what the installer mints with openssl (deploy/lib/scaler_tls.sh,
// llmsm_scaler_new_ca and llmsm_scaler_new_leaf), so a Secret written here and
// one written by make are interchangeable.
const (
	// rsaBits is the key size of the CA and both leaves (openssl -newkey rsa:2048).
	rsaBits = 2048
	// CAValidity is how long a minted CA is valid (LLMSM_SCALER_CA_DAYS).
	CAValidity = 3650 * 24 * time.Hour
	// ServingValidity is how long a serving certificate is valid
	// (LLMSM_SCALER_LEAF_DAYS).
	ServingValidity = 365 * 24 * time.Hour
	// ClientValidity is how long KEDA's client certificate is valid: as long as
	// the CA (LLMSM_SCALER_CLIENT_DAYS), because KEDA reads it once, when it
	// first dials the scaler, and keeps it until its operator restarts.
	ClientValidity = CAValidity
	// RenewBefore is how long before its expiry a leaf is re-issued, and from
	// when an expiring CA is reported (LLMSM_SCALER_RENEW_DAYS).
	RenewBefore = 30 * 24 * time.Hour
	// backdate puts NotBefore a little in the past, so a node whose clock runs
	// behind the controller's does not see a certificate that is not valid yet.
	backdate = 5 * time.Minute
)

// pemCertificate is the PEM block type of a certificate.
const pemCertificate = "CERTIFICATE"

// keyPair is a certificate with its private key, parsed and in PEM.
type keyPair struct {
	cert    *x509.Certificate
	signer  crypto.Signer
	certPEM []byte
	keyPEM  []byte
}

// tlsCertificate returns the pair as a tls.Certificate with Leaf set.
func (k *keyPair) tlsCertificate() *tls.Certificate {
	return &tls.Certificate{Certificate: [][]byte{k.cert.Raw}, PrivateKey: k.signer, Leaf: k.cert}
}

// parsePair parses a PEM certificate and its PEM private key, and fails when
// the key is not the certificate's.
func parsePair(certPEM, keyPEM []byte) (*keyPair, error) {
	if len(certPEM) == 0 {
		return nil, errors.New("no tls.crt")
	}
	if len(keyPEM) == 0 {
		return nil, errors.New("no tls.key")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	cert := pair.Leaf
	if cert == nil {
		if cert, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return nil, err
		}
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("tls.key holds a %T, which cannot sign", pair.PrivateKey)
	}
	return &keyPair{cert: cert, signer: signer, certPEM: certPEM, keyPEM: keyPEM}, nil
}

// firstCert parses the first PEM certificate in b.
func firstCert(b []byte) (*x509.Certificate, error) {
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			return nil, errors.New("no PEM certificate")
		}
		if block.Type == pemCertificate {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

// subjectKeyID is the SHA-1 of the subject public key bits (RFC 5280
// 4.2.1.2, method 1), as openssl's subjectKeyIdentifier=hash computes it. Go
// fills it in for a CA, not for a leaf.
func subjectKeyID(pub any) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(der, &spki); err != nil {
		return nil, err
	}
	sum := sha1.Sum(spki.PublicKey.Bytes) //nolint:gosec // an identifier, not a signature: RFC 5280 names SHA-1
	return sum[:], nil
}

// newSerial returns a random positive 128-bit serial number (openssl rand -hex 16).
func newSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	if n.Sign() == 0 {
		n.SetInt64(1)
	}
	return n, nil
}

// encode returns the PEM of a DER certificate and of a key, the key as PKCS #8
// ("PRIVATE KEY"), which is what openssl 3 writes.
func encode(der []byte, key *rsa.PrivateKey) (certPEM, keyPEM []byte, err error) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), nil
}

// mintCA mints a self-signed CA for namespace ns: CN llmsm-scaler-ca.<ns>,
// basicConstraints critical CA:TRUE, keyUsage critical keyCertSign and
// cRLSign, a subject key identifier.
func mintCA(ns string, now time.Time) (*keyPair, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "llmsm-scaler-ca." + ns},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(CAValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SignatureAlgorithm:    x509.SHA256WithRSA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := encode(der, key)
	if err != nil {
		return nil, err
	}
	return parsePair(certPEM, keyPEM)
}

// issueLeaf issues a leaf signed by ca: common name cn, DNS names dns,
// basicConstraints critical CA:FALSE, keyUsage critical digitalSignature and
// keyEncipherment, extended key usage eku, and the subject and authority key
// identifiers openssl 3 adds by default.
func issueLeaf(ca *keyPair, cn string, dns []string, eku x509.ExtKeyUsage, validity time.Duration, now time.Time) (*keyPair, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	ski, err := subjectKeyID(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		SubjectKeyId:          ski,
		Subject:               pkix.Name{CommonName: cn},
		DNSNames:              slices.Clone(dns),
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(validity),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{eku},
		SignatureAlgorithm:    x509.SHA256WithRSA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.signer)
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := encode(der, key)
	if err != nil {
		return nil, err
	}
	return parsePair(certPEM, keyPEM)
}

// checkLeaf says why leaf may not be kept, or returns nil: it must chain to ca
// for eku, stay valid for more than RenewBefore, and name exactly the DNS
// names in want (llmsm_scaler_cert_ok).
func checkLeaf(leaf *keyPair, ca *x509.Certificate, want []string, eku x509.ExtKeyUsage, now time.Time) error {
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.cert.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{eku},
	}); err != nil {
		return fmt.Errorf("does not chain to the CA: %w", err)
	}
	if !leaf.cert.NotAfter.After(now.Add(RenewBefore)) {
		return fmt.Errorf("expires %s, within %s", leaf.cert.NotAfter.UTC().Format(time.RFC3339), days(RenewBefore))
	}
	if got := sortedUnique(leaf.cert.DNSNames); !slices.Equal(got, sortedUnique(want)) {
		return fmt.Errorf("names %v, not %v", got, sortedUnique(want))
	}
	return nil
}

// checkCA says why ca cannot sign or be trusted, or returns nil.
func checkCA(ca *x509.Certificate, now time.Time) error {
	if !ca.IsCA || !ca.BasicConstraintsValid {
		return errors.New("is not a CA certificate")
	}
	if !now.Before(ca.NotAfter) {
		return fmt.Errorf("expired %s", ca.NotAfter.UTC().Format(time.RFC3339))
	}
	return nil
}

// days formats d in whole days.
func days(d time.Duration) string {
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}

// pemOf is the PEM of cert alone: what is written as ca.crt or published,
// never the bytes of a Secret field, which could hold more than the
// certificate.
func pemOf(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: cert.Raw})
}
