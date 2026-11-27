package scaler_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

// The install whose watchNamespaces setting is live refuses KEDA's calls for any
// other namespace before they register anything: the controller never
// manages a workload there. Without Admit every namespace is served, as on
// every other install.
var _ = Describe("Server namespace admission", func() {
	const refusedNS = "tenant-x"

	refRefused := func() *pb.ScaledObjectRef {
		return &pb.ScaledObjectRef{Namespace: refusedNS, Name: "chat-decode"}
	}

	dial := func(ctx context.Context, srv *scaler.Server) (pb.ExternalScalerClient, <-chan error, func()) {
		addr, done := startServer(ctx, srv)
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		Expect(err).NotTo(HaveOccurred())
		return pb.NewExternalScalerClient(conn), done, func() { Expect(conn.Close()).To(Succeed()) }
	}

	It("refuses every call from a namespace Admit refuses, and registers nothing", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reg := registry.New(time.Minute)
		srv := &scaler.Server{Registry: reg, Admit: func(_ context.Context, ns, _ string) (string, error) {
			if ns == refusedNS {
				return "it is not labelled", nil
			}
			return "", nil
		}}
		cli, done, closeConn := dial(ctx, srv)
		defer closeConn()

		By("an admitted namespace is served and registered")
		Eventually(whileServing(done, func() error {
			_, err := cli.GetMetricSpec(ctx, ref("chat-decode", nil))
			return err
		}), 5*time.Second).Should(Succeed())
		_, ok := reg.Get(testNamespace, "chat-decode")
		Expect(ok).To(BeTrue(), "the admitted ScaledObject was not registered")

		By("a refused namespace gets PermissionDenied on every call, with the reason")
		_, err := cli.GetMetricSpec(ctx, refRefused())
		Expect(status.Code(err)).To(Equal(codes.PermissionDenied), "GetMetricSpec: %v", err)
		Expect(err.Error()).To(ContainSubstring("it is not labelled"))
		_, err = cli.IsActive(ctx, refRefused())
		Expect(status.Code(err)).To(Equal(codes.PermissionDenied), "IsActive: %v", err)
		_, err = cli.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: refRefused()})
		Expect(status.Code(err)).To(Equal(codes.PermissionDenied), "GetMetrics: %v", err)
		stream, err := cli.StreamIsActive(ctx, refRefused())
		Expect(err).NotTo(HaveOccurred())
		_, err = stream.Recv()
		Expect(status.Code(err)).To(Equal(codes.PermissionDenied), "StreamIsActive: %v", err)

		_, ok = reg.Get(refusedNS, "chat-decode")
		Expect(ok).To(BeFalse(), "a refused ScaledObject was registered")
		cancel()
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
	})

	It("answers Unavailable when admission cannot be decided, and registers nothing", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reg := registry.New(time.Minute)
		srv := &scaler.Server{Registry: reg, Admit: func(context.Context, string, string) (string, error) {
			return "", errors.New("apiserver blip")
		}}
		cli, done, closeConn := dial(ctx, srv)
		defer closeConn()
		Eventually(whileServing(done, func() error {
			_, err := cli.GetMetricSpec(ctx, ref("chat-decode", nil))
			if status.Code(err) == codes.Unavailable && err != nil {
				return nil
			}
			return errors.New("not Unavailable yet: " + status.Convert(err).Message())
		}), 5*time.Second).Should(Succeed())
		Expect(reg.Len()).To(BeZero())
		cancel()
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
	})

	It("serves and registers every namespace without Admit", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reg := registry.New(time.Minute)
		cli, done, closeConn := dial(ctx, &scaler.Server{Registry: reg})
		defer closeConn()
		Eventually(whileServing(done, func() error {
			_, err := cli.GetMetricSpec(ctx, refRefused())
			return err
		}), 5*time.Second).Should(Succeed())
		_, ok := reg.Get(refusedNS, "chat-decode")
		Expect(ok).To(BeTrue())
		cancel()
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
	})
})
