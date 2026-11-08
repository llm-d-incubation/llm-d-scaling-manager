package scaler_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

// startServer runs srv.Start on a free loopback port and waits until it is
// listening, returning the address and the channel Start reports on.
//
// The port comes from below the kernel's ephemeral range, never from it.
// These specs used to bind 127.0.0.1:0, close the listener and hand the port
// to Addr. Port 0 draws from ip_local_port_range, which is also where every
// outgoing connection takes its source port and where those connections
// leave their TIME_WAIT sockets. Beside an envtest suite -- etcd and
// kube-apiserver churn loopback connections -- that range fills: measured on
// a WSL box (4096-port range) with 3950 of its ports in TIME_WAIT. The few
// ports left free are the ones the next connect() is likely to pick, so the
// port was taken between the close and Start's bind, Start failed with EADDRINUSE,
// and the spec dialled a port nobody listened on for five seconds and
// reported only "connection refused". At that fill even binding port 0 fails.
// A port below the range is never handed out by the kernel, so only another
// explicit bind can hold it; one that does is retried on another port.
func startServer(ctx context.Context, srv *scaler.Server) (string, <-chan error) {
	GinkgoHelper()
	var lastErr error
	for range portAttempts {
		// The probe is a plain bind, without SO_REUSEPORT, so it fails on any
		// socket holding the port -- including a listener that does set
		// SO_REUSEPORT, which the scaler's own bind would silently share the
		// port with.
		probe := listenUnassigned()
		addr := probe.Addr().String()
		Expect(probe.Close()).To(Succeed())

		srv.Addr = addr
		done := make(chan error, 1)
		go func() { done <- srv.Start(ctx) }()
		startErr := listening(srv, done)
		if startErr == nil {
			return addr, done
		}
		Expect(startErr).To(MatchError(syscall.EADDRINUSE), "the server's Start returned before it was listening")
		lastErr = startErr
	}
	Fail(fmt.Sprintf("the server's Start found its port taken %d times in a row: %v", portAttempts, lastErr))
	return "", nil
}

// listenUnassigned listens on a loopback port below the ephemeral range,
// trying up to portAttempts ports.
func listenUnassigned() net.Listener {
	GinkgoHelper()
	var lastErr error
	for range portAttempts {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(unassignedPort())))
		if err == nil {
			return l
		}
		lastErr = err
	}
	Fail(fmt.Sprintf("no free loopback port below the ephemeral range after %d attempts: %v", portAttempts, lastErr))
	return nil
}

// listening waits until srv reports Ready and returns nil, or returns the
// error Start returned first. Start returns only on a failure or on ctx, and
// ctx is not cancelled yet, so a return here is always a failure.
func listening(srv *scaler.Server, done <-chan error) error {
	GinkgoHelper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			Expect(err).To(HaveOccurred(), "the server's Start returned nil before it was listening")
			return err
		default:
		}
		if srv.Ready(nil) == nil {
			return nil
		}
		if time.Now().After(deadline) {
			Fail("the server neither listened nor returned from Start within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// portAttempts bounds how many ports startServer tries. A port below the
// ephemeral range is held only by something that bound it by number, so one
// collision is rare and several in a row mean the band itself is in use.
const portAttempts = 10

// unassignedPort picks a port in the band below the kernel's ephemeral range
// (the lower half of [0, first ephemeral port)), which no autobind -- port 0
// or a connect() -- ever hands out. Where the range cannot be read (not
// Linux) it assumes Linux's default first port, 32768, which also sits below
// the BSD/macOS and Windows ranges (49152 and up).
func unassignedPort() int {
	first := 32768
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
		if f := strings.Fields(string(b)); len(f) == 2 {
			if n, err := strconv.Atoi(f[0]); err == nil {
				first = n
			}
		}
	}
	return first/2 + rand.IntN(first-first/2)
}

// whileServing wraps a polled call so that a server whose Start has returned
// ends the poll with Start's error instead of being dialled until the poll
// times out. Start returns only on a failure or on ctx, and a spec that
// polls has not cancelled ctx yet, so either way nothing is listening.
func whileServing(done <-chan error, call func() error) func() error {
	return func() error {
		select {
		case err := <-done:
			return StopTrying("the server's Start returned while the spec was still calling it").Wrap(err)
		default:
		}
		return call()
	}
}

// A panic in one call must cost that call, not the process: the server is in
// the same binary as every engine, so an unrecovered panic in one workload's
// GetMetrics stops scaling for the whole fleet.
var _ = Describe("Server panic recovery", func() {
	It("answers a panicking call with Internal and keeps serving", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		promReg := prometheus.NewRegistry()
		Expect(metrics.InitMetrics(promReg)).To(Succeed())

		// No client: resolving the scale target dereferences it, which panics in
		// the handler goroutine -- the stand-in for any bug in a handler.
		srv := &scaler.Server{Registry: registry.New(time.Minute)}
		addr, done := startServer(ctx, srv)

		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(conn.Close()).To(Succeed()) }()
		cli := pb.NewExternalScalerClient(conn)

		Eventually(whileServing(done, func() error {
			_, err := cli.GetMetricSpec(ctx, ref("chat-decode", nil))
			return err
		}), 5*time.Second).Should(Succeed())

		By("unary: GetMetrics panics and is answered Internal")
		_, err = cli.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref("chat-decode", nil)})
		Expect(status.Code(err)).To(Equal(codes.Internal), "got %v", err)

		By("stream: StreamIsActive panics and is answered Internal")
		stream, err := cli.StreamIsActive(ctx, ref("chat-decode", nil))
		Expect(err).NotTo(HaveOccurred())
		_, err = stream.Recv()
		Expect(status.Code(err)).To(Equal(codes.Internal), "got %v", err)

		By("each recovered panic is counted under its method")
		Expect(panicsCounted(promReg)).To(Equal(map[string]float64{
			"/externalscaler.ExternalScaler/GetMetrics":     1,
			"/externalscaler.ExternalScaler/StreamIsActive": 1,
		}))

		By("the server is still up")
		_, err = cli.GetMetricSpec(ctx, ref("chat-decode", nil))
		Expect(err).NotTo(HaveOccurred())

		cancel()
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
	})
})

// panicsCounted reads llmsm_scaler_panics_total from reg, by method.
func panicsCounted(reg *prometheus.Registry) map[string]float64 {
	GinkgoHelper()
	mfs, err := reg.Gather()
	Expect(err).NotTo(HaveOccurred())
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != constants.LLMSMScalerPanicsTotal {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == constants.LabelMethod {
					out[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}
	return out
}

// The Server runs in two phases over one address: a standby phase until the
// lease is held, then the full server on a fresh listener. This spec drives a
// real gRPC client through both, because the hand-off -- stop one server,
// bind again, serve the other -- is the part a handler-level spec cannot see.
var _ = Describe("Server before and after the leader lease", func() {
	It("advertises the metric spec before the lease, refuses decisions, then serves them", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		s := runtime.NewScheme()
		Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(scaledObjectFor("chat-decode", "chat-decode-deploy")).Build()
		// The handler reads decision.Default when the Server builds it; seed
		// that rather than a private store so the post-election answer is real.
		decision.Default.Set(testNamespace, "chat-decode-deploy", 3)

		elected := make(chan struct{})
		srv := &scaler.Server{Client: c, Registry: registry.New(time.Minute), Elected: elected}
		Expect(srv.Ready(nil)).To(HaveOccurred(), "not listening yet, so not Ready")
		addr, done := startServer(ctx, srv)

		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(conn.Close()).To(Succeed()) }()
		cli := pb.NewExternalScalerClient(conn)

		By("standby: GetMetricSpec is answered, and the pod reads Ready")
		Eventually(whileServing(done, func() error {
			_, err := cli.GetMetricSpec(ctx, ref("chat-decode", nil))
			return err
		}), 5*time.Second).Should(Succeed())
		Expect(srv.Ready(nil)).To(Succeed())

		By("standby: GetMetrics and IsActive are refused with Unavailable")
		_, err = cli.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref("chat-decode", nil)})
		Expect(status.Code(err)).To(Equal(codes.Unavailable), "got %v", err)
		_, err = cli.IsActive(ctx, ref("chat-decode", nil))
		Expect(status.Code(err)).To(Equal(codes.Unavailable), "got %v", err)

		By("election: the same address serves decisions to the same client")
		close(elected)
		// The standby server stops and the full one binds the port again; the
		// client's connection is closed by the hand-off and re-dialled by its
		// next call, so the first attempts may see the gap.
		var value int64
		Eventually(whileServing(done, func() error {
			m, err := cli.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref("chat-decode", nil)})
			if err != nil {
				return err
			}
			value = m.MetricValues[0].MetricValue
			return nil
		}), 5*time.Second).Should(Succeed())
		Expect(value).To(Equal(int64(3)))

		Expect(srv.Ready(nil)).To(Succeed(), "still Ready across the hand-off")

		By("shutdown: a stream KEDA never hangs up on does not hold the stop open")
		// A push trigger keeps StreamIsActive open indefinitely; a plain
		// GracefulStop would wait for it through the manager's whole shutdown
		// budget, and the leader lease is released only after that. The stop
		// is bounded, so this returns in stopGrace rather than never.
		// Not derived from ctx: cancelling the server must not be what ends
		// the stream, or the stop is never actually waiting on it.
		streamCtx, stopStream := context.WithCancel(context.Background())
		defer stopStream()
		stream, err := cli.StreamIsActive(streamCtx, ref("chat-decode", nil))
		Expect(err).NotTo(HaveOccurred())
		first, err := stream.Recv()
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Result).To(BeTrue())

		stopped := time.Now()
		cancel()
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
		Expect(time.Since(stopped)).To(BeNumerically("<", 8*time.Second), "the stop waited on the open stream")
		Expect(srv.Ready(nil)).To(HaveOccurred(), "stopped, so not Ready")
		_, err = stream.Recv()
		Expect(err).To(HaveOccurred(), "the stream was closed by the forced stop")
	})
})
