package scaler_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"strings"
	"sync"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// A client the scaler refuses at the TLS layer used to leave nothing in the
// controller's log: on a real cluster, a ScaledObject without the
// authenticationRef, a wrong SAN and a foreign CA were each visible only in
// KEDA's operator log.
var _ = Describe("a refused TLS handshake is logged", func() {
	// logCapture collects the lines a server logs from its own goroutines.
	type logCapture struct {
		mu    sync.Mutex
		lines []string
	}
	refusals := func(c *logCapture) []string {
		c.mu.Lock()
		defer c.mu.Unlock()
		var out []string
		for _, l := range c.lines {
			if strings.Contains(l, "refused a TLS handshake") {
				out = append(out, l)
			}
		}
		return out
	}

	DescribeTable("names the remote and the reason, once per remote per minute",
		func(client func(server, clientCA *testCA) credentials.TransportCredentials, reason string) {
			c := &logCapture{}
			lg := funcr.NewJSON(func(obj string) {
				c.mu.Lock()
				defer c.mu.Unlock()
				c.lines = append(c.lines, obj)
			}, funcr.Options{})
			ctx, cancel := context.WithCancel(ctrllog.IntoContext(context.Background(), lg))
			defer cancel()
			server, clientCA := newTestCA(), newTestCA()
			addr := startTLSServer(ctx, server, clientCA, "scaling-manager-keda-client")

			Expect(metricSpec(ctx, addr, client(server, clientCA))).To(HaveOccurred(), "the client reached a call")
			Eventually(func() []string { return refusals(c) }).Should(HaveLen(1), "no line for the refused handshake")
			line := refusals(c)[0]
			Expect(line).To(ContainSubstring(`"remote":"127.0.0.1:`))
			Expect(line).To(ContainSubstring(reason))

			By("a second refusal from the same host within the minute adds no line")
			Expect(metricSpec(ctx, addr, client(server, clientCA))).To(HaveOccurred())
			Consistently(func() []string { return refusals(c) }, "300ms").Should(HaveLen(1))
		},
		Entry("no client certificate", func(server, _ *testCA) credentials.TransportCredentials {
			return credentials.NewTLS(&tls.Config{RootCAs: poolOf(server), MinVersion: tls.VersionTLS12})
		}, "certificate"),
		Entry("a certificate from another CA", func(server, _ *testCA) credentials.TransportCredentials {
			return credentials.NewTLS(&tls.Config{RootCAs: poolOf(server), Certificates: []tls.Certificate{pairOf(newTestCA(), "scaling-manager-keda-client")}, MinVersion: tls.VersionTLS12})
		}, "unknown authority"),
		Entry("the client CA, another identity", func(server, clientCA *testCA) credentials.TransportCredentials {
			return credentials.NewTLS(&tls.Config{RootCAs: poolOf(server), Certificates: []tls.Certificate{pairOf(clientCA, "intruder")}, MinVersion: tls.VersionTLS12})
		}, "not in the allowed client list"),
		Entry("a plaintext client", func(_, _ *testCA) credentials.TransportCredentials {
			return insecure.NewCredentials()
		}, "tls:"),
	)
})

// poolOf is a pool trusting ca.
func poolOf(ca *testCA) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// pairOf is a client key pair ca signed for the DNS name san.
func pairOf(ca *testCA, san string) tls.Certificate {
	GinkgoHelper()
	certPEM, keyPEM := ca.leaf(20, x509.ExtKeyUsageClientAuth, func(c *x509.Certificate) { c.DNSNames = []string{san} })
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	Expect(err).NotTo(HaveOccurred())
	return pair
}
