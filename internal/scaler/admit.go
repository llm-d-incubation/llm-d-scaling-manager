package scaler

import (
	"context"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// AdmitFunc says whether the scaler serves ScaledObject scaledObject in
// namespace. refusal is why it does not, "" when it does. err is a check that
// could not be made; the call is then refused as Unavailable, which KEDA
// retries, and never served.
type AdmitFunc func(ctx context.Context, namespace, scaledObject string) (refusal string, err error)

// admitting refuses every call from a namespace admit refuses, before the
// call reaches the handler: a refused ScaledObject is never registered, so
// the controller never manages its workload. A nil ref passes through, and
// the handler rejects it as it always did.
type admitting struct {
	pb.UnimplementedExternalScalerServer
	inner pb.ExternalScalerServer
	admit AdmitFunc
}

// withAdmission wraps h with admit; a nil admit returns h unchanged.
func withAdmission(h pb.ExternalScalerServer, admit AdmitFunc) pb.ExternalScalerServer {
	if admit == nil {
		return h
	}
	return &admitting{inner: h, admit: admit}
}

// check returns the gRPC error that refuses ref, or nil.
func (a *admitting) check(ctx context.Context, ref *pb.ScaledObjectRef, call string) error {
	if ref == nil {
		return nil
	}
	refusal, err := a.admit(ctx, ref.GetNamespace(), ref.GetName())
	if err != nil {
		return status.Errorf(codes.Unavailable, "cannot tell whether namespace %s is managed: %v", ref.GetNamespace(), err)
	}
	if refusal == "" {
		return nil
	}
	log.FromContext(ctx).V(1).Info("external scaler refused a call from a namespace this controller does not manage",
		"call", call, "namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "reason", refusal)
	return status.Errorf(codes.PermissionDenied, "namespace %s is not managed by this scaling manager: %s",
		ref.GetNamespace(), refusal)
}

func (a *admitting) IsActive(ctx context.Context, ref *pb.ScaledObjectRef) (*pb.IsActiveResponse, error) {
	if err := a.check(ctx, ref, "IsActive"); err != nil {
		return nil, err
	}
	return a.inner.IsActive(ctx, ref)
}

func (a *admitting) StreamIsActive(ref *pb.ScaledObjectRef, stream pb.ExternalScaler_StreamIsActiveServer) error {
	if err := a.check(stream.Context(), ref, "StreamIsActive"); err != nil {
		return err
	}
	return a.inner.StreamIsActive(ref, &admittedStream{ExternalScaler_StreamIsActiveServer: stream, ref: ref, a: a})
}

// admittedStream checks the namespace again before each message on an open
// StreamIsActive stream, so a namespace left out of the setting after the
// stream opened is refused at the next push or keepalive, not served until
// KEDA reopens the stream.
type admittedStream struct {
	pb.ExternalScaler_StreamIsActiveServer
	ref *pb.ScaledObjectRef
	a   *admitting
}

func (s *admittedStream) Send(resp *pb.IsActiveResponse) error {
	if err := s.a.check(s.Context(), s.ref, "StreamIsActive"); err != nil {
		return err
	}
	return s.ExternalScaler_StreamIsActiveServer.Send(resp)
}

func (a *admitting) GetMetricSpec(ctx context.Context, ref *pb.ScaledObjectRef) (*pb.GetMetricSpecResponse, error) {
	if err := a.check(ctx, ref, "GetMetricSpec"); err != nil {
		return nil, err
	}
	return a.inner.GetMetricSpec(ctx, ref)
}

func (a *admitting) GetMetrics(ctx context.Context, req *pb.GetMetricsRequest) (*pb.GetMetricsResponse, error) {
	if err := a.check(ctx, req.GetScaledObjectRef(), "GetMetrics"); err != nil {
		return nil, err
	}
	return a.inner.GetMetrics(ctx, req)
}
