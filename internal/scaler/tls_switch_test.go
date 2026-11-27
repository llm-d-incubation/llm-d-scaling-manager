package scaler_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

// A TLS configuration sent on TLSSwitch is served from the next connection
// on, with no restart of the process: a plaintext client is refused after
// the switch, and one that verifies the certificate is served.
var _ = Describe("Server TLS switch", func() {
	It("serves plaintext, then TLS after a switch, without restarting", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		switches := make(chan *tls.Config, 1)
		srv := &scaler.Server{Registry: registry.New(time.Minute), TLSSwitch: switches}
		addr, done := startServer(ctx, srv)

		By("plaintext before the switch")
		Eventually(whileServing(done, func() error { return metricSpec(ctx, addr, insecure.NewCredentials()) }),
			5*time.Second).Should(Succeed())

		By("TLS after it")
		ca := newTestCA()
		certPEM, keyPEM := ca.leaf(2, x509.ExtKeyUsageServerAuth)
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		Expect(err).NotTo(HaveOccurred())
		switches <- &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
		roots := x509.NewCertPool()
		roots.AddCert(ca.cert)
		tlsCreds := credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
		Eventually(whileServing(done, func() error { return metricSpec(ctx, addr, tlsCreds) }),
			10*time.Second).Should(Succeed())
		Expect(metricSpec(ctx, addr, insecure.NewCredentials())).To(HaveOccurred(),
			"a plaintext client reached an RPC after the switch to TLS")
		Consistently(done, time.Second).ShouldNot(Receive(), "the server's Start returned during the switch")

		cancel()
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
	})
})
