package scaler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// reachWindow is how long the scaler must have served nothing, while it kept
// refusing TLS handshakes, before it reports that no client reaches it. It
// matches the window the installer reads the refusal log over
// (LLMSM_SCALER_REFUSAL_SINCE).
const reachWindow = 5 * time.Minute

// reachConfirm is how long refusals must have gone on, with nothing served in
// between, before they count. A client that is refused once and then
// connects is not "unreachable".
const reachConfirm = time.Minute

// reachCheckInterval is how often Server re-evaluates reachability.
const reachCheckInterval = 15 * time.Second

// reachability tracks whether any client reaches the scaler over TLS: the
// handshakes it refused, and the handshakes and calls it served.
//
// KEDA pools one gRPC connection per scalerAddress and never closes it, so a
// KEDA holding a client certificate or CA from an earlier install is refused
// on every dial and reaches no call. The refusals alone are an INFO line per
// minute, and the controller otherwise looks healthy: it simply sees no
// model. unreachable is what turns that into a status condition and a metric.
type reachability struct {
	mu  sync.Mutex
	now func() time.Time

	// lastServed is the last completed handshake, or the last call started.
	lastServed time.Time
	// streams counts the calls in flight: an open StreamIsActive is a client
	// that reaches the scaler, however long ago it started.
	streams int
	// firstRefused is the first refusal since lastServed; zero when none.
	firstRefused time.Time
	// lastRefused is the most recent refusal.
	lastRefused time.Time
	// refused counts the refusals since lastServed.
	refused int
	// lastReason is the handshake error of the most recent refusal.
	lastReason string
}

func newReachability(now func() time.Time) *reachability {
	if now == nil {
		now = time.Now
	}
	return &reachability{now: now}
}

// served records a completed handshake or a call: some client reaches the
// scaler, which ends any run of refusals.
func (r *reachability) served() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastServed = r.now()
	r.firstRefused = time.Time{}
	r.refused = 0
}

// begin records a call that starts, and returns the func that records its end.
func (r *reachability) begin() func() {
	r.mu.Lock()
	r.lastServed = r.now()
	r.firstRefused = time.Time{}
	r.refused = 0
	r.streams++
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.streams--
		r.lastServed = r.now()
		r.mu.Unlock()
	}
}

// refusedHandshake records one refused handshake and its reason.
func (r *reachability) refusedHandshake(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.firstRefused.IsZero() {
		r.firstRefused = now
	}
	r.lastRefused = now
	r.refused++
	r.lastReason = reason
}

// state reports whether no client reaches the scaler: a handshake was refused
// within reachWindow, refusals have gone on for at least reachConfirm, and
// nothing was served within reachWindow. detail says how many were refused and
// the last reason, for the status condition.
func (r *reachability) state() (unreachable bool, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	switch {
	case r.streams > 0,
		r.firstRefused.IsZero(),
		now.Sub(r.lastRefused) > reachWindow,
		!r.lastServed.IsZero() && now.Sub(r.lastServed) < reachWindow,
		now.Sub(r.firstRefused) < reachConfirm:
		return false, ""
	}
	return true, fmt.Sprintf("the KEDA external scaler refused %d TLS handshake(s) since %s and served no call in the last %s (last refusal: %s). "+
		"KEDA reaches no call, so no model is discovered and nothing scales. After an uninstall and reinstall in this namespace, "+
		"KEDA's operator still holds its connection from before: restart it (kubectl rollout restart deployment/keda-operator -n <KEDA namespace>). "+
		"Otherwise make verify-deployment names the cause",
		r.refused, r.firstRefused.UTC().Format(time.RFC3339), reachWindow, r.lastReason)
}

// unaryInterceptor records each unary call as served.
func (r *reachability) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	r.served()
	return handler(ctx, req)
}

// streamInterceptor records a stream as served for as long as it is open.
func (r *reachability) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	end := r.begin()
	defer end()
	return handler(srv, ss)
}

// reachReporter publishes reachability changes: the metric, a log line, and
// the hook (the controller-status condition).
type reachReporter struct {
	reach  *reachability
	hook   func(unreachable bool, detail string)
	logger logr.Logger
	// last is the state last published; nil before the first.
	last *bool
}

// check evaluates reachability once and publishes it when it changed.
func (p *reachReporter) check() {
	unreachable, detail := p.reach.state()
	if p.last != nil && *p.last == unreachable {
		// The detail moves with every refusal; the condition is rewritten
		// only when it flips, so a quiet controller does not write.
		return
	}
	prev := p.last
	p.last = &unreachable
	metrics.SetScalerKEDAUnreachable(unreachable)
	if unreachable {
		p.logger.Error(nil, "No client reaches the KEDA external scaler: every TLS handshake is refused", "detail", detail)
	} else if prev != nil {
		p.logger.Info("A client reaches the KEDA external scaler again")
	}
	if p.hook != nil {
		p.hook(unreachable, detail)
	}
}

// run checks every interval until ctx is done.
func (p *reachReporter) run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	p.check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.check()
		}
	}
}
