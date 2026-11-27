package scaler

import (
	"context"
	"sync/atomic"
	"testing"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sentStream counts what reaches the client of a StreamIsActive stream.
type sentStream struct {
	pb.ExternalScaler_StreamIsActiveServer
	sent int
}

func (s *sentStream) Context() context.Context { return context.Background() }

func (s *sentStream) Send(*pb.IsActiveResponse) error {
	s.sent++
	return nil
}

// A namespace left out after its StreamIsActive opened is refused at the
// stream's next message: the open stream does not go on serving it until
// KEDA reopens it.
func TestAdmittedStreamRefusesOnceTheNamespaceIsLeftOut(t *testing.T) {
	var managed atomic.Bool
	managed.Store(true)
	a := &admitting{admit: func(context.Context, string, string) (string, error) {
		if managed.Load() {
			return "", nil
		}
		return "it is not in the watchNamespaces setting", nil
	}}
	inner := &sentStream{}
	s := &admittedStream{ExternalScaler_StreamIsActiveServer: inner,
		ref: &pb.ScaledObjectRef{Namespace: "team-a", Name: "chat-decode"}, a: a}

	if err := s.Send(&pb.IsActiveResponse{Result: true}); err != nil || inner.sent != 1 {
		t.Fatalf("a managed namespace: err %v, sent %d", err, inner.sent)
	}
	managed.Store(false)
	err := s.Send(&pb.IsActiveResponse{Result: true})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("after the namespace was left out: err %v, want PermissionDenied", err)
	}
	if inner.sent != 1 {
		t.Errorf("a message reached KEDA after the namespace was left out: sent %d", inner.sent)
	}
}
