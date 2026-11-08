package scaler_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

// testCA is a throwaway certificate authority that signs the server and client
// leaves a TLS spec needs.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA() *testCA {
	GinkgoHelper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	Expect(err).NotTo(HaveOccurred())
	cert, err := x509.ParseCertificate(der)
	Expect(err).NotTo(HaveOccurred())
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// leaf returns a PEM certificate and key signed by the CA.
func (ca *testCA) leaf(serial int64, usage x509.ExtKeyUsage, opts ...func(*x509.Certificate)) (certPEM, keyPEM []byte) {
	GinkgoHelper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	for _, o := range opts {
		o(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	Expect(err).NotTo(HaveOccurred())
	keyDER, err := x509.MarshalECPrivateKey(key)
	Expect(err).NotTo(HaveOccurred())
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// startTLSServer runs an elected Server over TLS and returns its address.
// clientCA, when non-nil, is the CA client certificates must chain to, and
// allowed the client identities accepted among them.
func startTLSServer(ctx context.Context, ca *testCA, clientCA *testCA, allowed ...string) string {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	certPEM, keyPEM := ca.leaf(2, x509.ExtKeyUsageServerAuth)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	Expect(err).NotTo(HaveOccurred())
	caFile := ""
	if clientCA != nil {
		caFile = filepath.Join(dir, "client-ca.crt")
		Expect(os.WriteFile(caFile, clientCA.pem, 0o600)).To(Succeed())
	}
	cfg, err := scaler.NewTLSConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &pair, nil }, caFile, allowed)
	Expect(err).NotTo(HaveOccurred())

	s := runtime.NewScheme()
	Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(scaledObjectFor("chat-decode", "chat-decode-deploy")).Build()
	srv := &scaler.Server{Client: c, Registry: registry.New(time.Minute), TLSConfig: cfg}
	addr, _ := startServer(ctx, srv)
	return addr
}

// metricSpec makes one GetMetricSpec call over a fresh connection.
func metricSpec(ctx context.Context, addr string, creds credentials.TransportCredentials) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err = pb.NewExternalScalerClient(conn).GetMetricSpec(callCtx, ref("chat-decode", nil))
	return err
}

var _ = Describe("Server over TLS", func() {
	It("refuses a plaintext client and serves one that verifies the certificate", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ca := newTestCA()
		addr := startTLSServer(ctx, ca, nil)

		Expect(metricSpec(ctx, addr, insecure.NewCredentials())).To(HaveOccurred(),
			"a plaintext client reached an RPC on a TLS server")

		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		Expect(metricSpec(ctx, addr, credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))).To(Succeed())
	})

	It("with a client CA, refuses a client without a certificate it signed", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ca := newTestCA()
		clientCA := newTestCA()
		addr := startTLSServer(ctx, ca, clientCA)
		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)

		By("no client certificate")
		Expect(metricSpec(ctx, addr, credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))).To(HaveOccurred())

		By("a client certificate from the wrong CA")
		wrongPEM, wrongKey := ca.leaf(3, x509.ExtKeyUsageClientAuth)
		wrong, err := tls.X509KeyPair(wrongPEM, wrongKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(metricSpec(ctx, addr, credentials.NewTLS(&tls.Config{
			RootCAs: pool, Certificates: []tls.Certificate{wrong}, MinVersion: tls.VersionTLS12,
		}))).To(HaveOccurred())

		By("a client certificate the client CA signed")
		goodPEM, goodKey := clientCA.leaf(4, x509.ExtKeyUsageClientAuth)
		good, err := tls.X509KeyPair(goodPEM, goodKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(metricSpec(ctx, addr, credentials.NewTLS(&tls.Config{
			RootCAs: pool, Certificates: []tls.Certificate{good}, MinVersion: tls.VersionTLS12,
		}))).To(Succeed())
	})

	It("rejects a client CA file with no certificate in it", func() {
		f := filepath.Join(GinkgoT().TempDir(), "empty.crt")
		Expect(os.WriteFile(f, []byte("not a certificate"), 0o600)).To(Succeed())
		_, err := scaler.NewTLSConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }, f, nil)
		Expect(err).To(HaveOccurred())
	})

	Describe("client identity allowlist", func() {
		// clientCreds is a TLS client that trusts ca and presents a leaf the
		// client CA signed, carrying the identities set by opt.
		clientCreds := func(ca, clientCA *testCA, serial int64, opt func(*x509.Certificate)) credentials.TransportCredentials {
			GinkgoHelper()
			pool := x509.NewCertPool()
			pool.AddCert(ca.cert)
			certPEM, keyPEM := clientCA.leaf(serial, x509.ExtKeyUsageClientAuth, opt)
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			Expect(err).NotTo(HaveOccurred())
			return credentials.NewTLS(&tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
		}
		dns := func(name string) func(*x509.Certificate) {
			return func(c *x509.Certificate) { c.DNSNames = []string{name} }
		}

		It("accepts a client the CA signed whose SAN is allowed, and refuses one with another SAN", func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ca, clientCA := newTestCA(), newTestCA()
			addr := startTLSServer(ctx, ca, clientCA,
				"keda-operator.keda.svc", "spiffe://cluster.local/ns/keda/sa/keda-operator")

			By("an allowed DNS SAN")
			Expect(metricSpec(ctx, addr, clientCreds(ca, clientCA, 10, dns("keda-operator.keda.svc")))).To(Succeed())

			By("an allowed URI SAN")
			spiffe, err := url.Parse("spiffe://cluster.local/ns/keda/sa/keda-operator")
			Expect(err).NotTo(HaveOccurred())
			Expect(metricSpec(ctx, addr, clientCreds(ca, clientCA, 11, func(c *x509.Certificate) {
				c.URIs = []*url.URL{spiffe}
			}))).To(Succeed())

			By("another SAN from the same CA")
			Expect(metricSpec(ctx, addr, clientCreds(ca, clientCA, 12, dns("intruder.default.svc")))).To(HaveOccurred(),
				"a certificate the client CA signed for another identity reached an RPC")
		})

		It("accepts a client by its subject Common Name", func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ca, clientCA := newTestCA(), newTestCA()
			addr := startTLSServer(ctx, ca, clientCA, "keda-operator")
			Expect(metricSpec(ctx, addr, clientCreds(ca, clientCA, 13, func(c *x509.Certificate) {
				c.Subject.CommonName = "keda-operator"
			}))).To(Succeed())
			// leaf's default CN is "leaf".
			Expect(metricSpec(ctx, addr, clientCreds(ca, clientCA, 14, func(*x509.Certificate) {}))).To(HaveOccurred())
		})

		It("is refused without a client CA", func() {
			_, err := scaler.NewTLSConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }, "", []string{"keda-operator"})
			Expect(err).To(MatchError(ContainSubstring("needs a client CA")))
		})

		It("parses a comma-separated list, dropping blanks", func() {
			Expect(scaler.ParseClientIdentities(" a.svc, ,spiffe://x/y ,")).To(Equal([]string{"a.svc", "spiffe://x/y"}))
			Expect(scaler.ParseClientIdentities("")).To(BeEmpty())
		})
	})

	Describe("the TLS flags", func() {
		It("accepts the installer's flags and returns the identities", func() {
			allowed, err := scaler.CheckTLSFlags(scaler.TLSFlags{
				CertPath: "/etc/scaling-manager/scaler-tls", ClientCA: "/ca.crt", ClientCASet: true,
				ClientSAN: "scaling-manager-keda-client", ClientSANSet: true,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(allowed).To(Equal([]string{"scaling-manager-keda-client"}))
		})

		It("accepts no TLS flags at all, and TLS without mutual TLS", func() {
			_, err := scaler.CheckTLSFlags(scaler.TLSFlags{})
			Expect(err).NotTo(HaveOccurred())
			_, err = scaler.CheckTLSFlags(scaler.TLSFlags{CertPath: "/etc/scaling-manager/scaler-tls"})
			Expect(err).NotTo(HaveOccurred())
		})

		DescribeTable("refuses a client SAN that names no identity",
			func(san string) {
				_, err := scaler.CheckTLSFlags(scaler.TLSFlags{
					CertPath: "/etc/scaling-manager/scaler-tls", ClientCA: "/ca.crt", ClientCASet: true,
					ClientSAN: san, ClientSANSet: true,
				})
				Expect(err).To(MatchError(ContainSubstring("names no client identity")))
			},
			Entry("a comma", ","),
			Entry("a space", " "),
			Entry("empty", ""),
		)

		It("refuses a client CA passed empty next to a serving certificate", func() {
			_, err := scaler.CheckTLSFlags(scaler.TLSFlags{CertPath: "/etc/scaling-manager/scaler-tls", ClientCA: " ", ClientCASet: true})
			Expect(err).To(MatchError(ContainSubstring("--external-scaler-client-ca was passed empty")))
		})

		It("refuses a client CA without a serving certificate, and a SAN without a client CA", func() {
			_, err := scaler.CheckTLSFlags(scaler.TLSFlags{ClientCA: "/ca.crt", ClientCASet: true})
			Expect(err).To(MatchError(ContainSubstring("needs --external-scaler-cert-path")))
			_, err = scaler.CheckTLSFlags(scaler.TLSFlags{CertPath: "/etc/scaling-manager/scaler-tls", ClientSAN: "x", ClientSANSet: true})
			Expect(err).To(MatchError(ContainSubstring("needs --external-scaler-client-ca")))
		})
	})

	It("serves a rotated certificate on the next handshake", func() {
		ca := newTestCA()
		certs := make([]tls.Certificate, 2)
		for i := range certs {
			certPEM, keyPEM := ca.leaf(int64(20+i), x509.ExtKeyUsageServerAuth)
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			Expect(err).NotTo(HaveOccurred())
			certs[i] = pair
		}
		// current stands in for certwatcher: what GetCertificate returns is
		// whatever was last loaded from the Secret.
		var current atomic.Pointer[tls.Certificate]
		current.Store(&certs[0])
		cfg, err := scaler.NewTLSConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return current.Load(), nil }, "", nil)
		Expect(err).NotTo(HaveOccurred())

		// Not 127.0.0.1:0: see startServer for why a port from the ephemeral
		// range is not reliably bindable here.
		ln := tls.NewListener(listenUnassigned(), cfg)
		defer func() { _ = ln.Close() }()
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_ = conn.(*tls.Conn).Handshake()
				_ = conn.Close()
			}
		}()

		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		served := func() *big.Int {
			GinkgoHelper()
			conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = conn.Close() }()
			return conn.ConnectionState().PeerCertificates[0].SerialNumber
		}
		Expect(served()).To(Equal(big.NewInt(20)))
		current.Store(&certs[1])
		Expect(served()).To(Equal(big.NewInt(21)), "a rotated certificate must be served without rebuilding the TLS config")
	})
})
