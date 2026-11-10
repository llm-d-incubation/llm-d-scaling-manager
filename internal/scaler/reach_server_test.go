package scaler_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

// The reachability tracker as the running Server wires it: a refused
// handshake, a completed one, a call and a stream each reach it through the
// gRPC server's credentials and interceptors, and the reporter publishes the
// starting state. The tracker's own rules are in reachability_internal_test.go.
var _ = Describe("Server reachability over mutual TLS", func() {
	It("counts a client that rejects the scaler, then a completed handshake, a call and a stream as served", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ca := newTestCA()
		clientCA := newTestCA()

		dir := GinkgoT().TempDir()
		certPEM, keyPEM := ca.leaf(2, x509.ExtKeyUsageServerAuth)
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		Expect(err).NotTo(HaveOccurred())
		caFile := filepath.Join(dir, "client-ca.crt")
		Expect(os.WriteFile(caFile, clientCA.pem, 0o600)).To(Succeed())
		cfg, err := scaler.NewTLSConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &pair, nil }, caFile, nil)
		Expect(err).NotTo(HaveOccurred())
		s := runtime.NewScheme()
		Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(scaledObjectFor("chat-decode", "chat-decode-deploy")).Build()
		published := make(chan bool, 16)
		srv := &scaler.Server{Client: c, Registry: registry.New(time.Minute), TLSConfig: cfg,
			OnReachability: func(unreachable bool, _ string) { published <- unreachable }}
		addr, _ := startServer(ctx, srv)

		By("the reporter runs, and publishes the starting state")
		Eventually(published).Should(Receive(BeFalse()))

		goodPEM, goodKey := clientCA.leaf(4, x509.ExtKeyUsageClientAuth)
		good, err := tls.X509KeyPair(goodPEM, goodKey)
		Expect(err).NotTo(HaveOccurred())
		serverPool := x509.NewCertPool()
		serverPool.AddCert(ca.cert)
		goodTLS := &tls.Config{RootCAs: serverPool, Certificates: []tls.Certificate{good}, MinVersion: tls.VersionTLS12}

		By("a client that trusts another CA rejects the scaler's certificate: a refusal")
		otherPool := x509.NewCertPool()
		otherPool.AddCert(newTestCA().cert)
		Expect(metricSpec(ctx, addr, credentials.NewTLS(&tls.Config{
			RootCAs: otherPool, Certificates: []tls.Certificate{good}, MinVersion: tls.VersionTLS12,
		}))).To(HaveOccurred())
		Eventually(func() int { n, _, _ := scaler.ReachOf(srv); return n }).Should(BeNumerically(">", 0),
			"a refused handshake did not reach the tracker")

		By("a handshake that completes, with no call: served")
		raw := goodTLS.Clone()
		raw.NextProtos = []string{"h2"}
		conn, err := tls.Dial("tcp", addr, raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(conn.Handshake()).To(Succeed())
		Eventually(func() bool { n, at, _ := scaler.ReachOf(srv); return n == 0 && !at.IsZero() }).Should(BeTrue(),
			"a completed handshake was not counted as served")
		_ = conn.Close()

		By("a call: served")
		_, before, _ := scaler.ReachOf(srv)
		time.Sleep(5 * time.Millisecond)
		Expect(metricSpec(ctx, addr, credentials.NewTLS(goodTLS))).To(Succeed())
		_, after, _ := scaler.ReachOf(srv)
		Expect(after.After(before)).To(BeTrue(), "a call did not move lastServed")

		By("a stream: open while it lasts, and its end counts as served")
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(goodTLS)))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = cc.Close() }()
		streamCtx, stop := context.WithCancel(ctx)
		_, err = pb.NewExternalScalerClient(cc).StreamIsActive(streamCtx, ref("chat-decode", nil))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int { _, _, n := scaler.ReachOf(srv); return n }).Should(Equal(1), "an open stream was not counted")
		_, opened, _ := scaler.ReachOf(srv)
		time.Sleep(5 * time.Millisecond)
		stop()
		Eventually(func() int { _, _, n := scaler.ReachOf(srv); return n }).Should(BeZero(), "a closed stream stayed counted")
		_, closed, _ := scaler.ReachOf(srv)
		Expect(closed.After(opened)).To(BeTrue(), "a stream's end did not count as served")
	})
})
