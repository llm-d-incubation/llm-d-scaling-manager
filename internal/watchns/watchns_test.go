package watchns

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// denying is a client whose lists in the namespaces of deny fail with the
// error deny maps them to.
func denying(deny map[string]error) client.Client {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err, ok := deny[(&client.ListOptions{}).ApplyOptions(opts).Namespace]; ok {
				return err
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
}

var (
	forbidden = apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", errors.New("RoleBinding deleted"))
	gone      = apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "team-c")
	blip      = apierrors.NewServiceUnavailable("api server restarting")
)

// One listed namespace that cannot be read must not keep the others from
// being managed: it is split out with its reason, and the rest are cached.
func TestSplitSeparatesTheUnreadable(t *testing.T) {
	c := denying(map[string]error{"team-b": forbidden, "team-c": gone, "team-d": blip})
	readable, blocked := Split(context.Background(), c, []string{"team-a", "team-b", "team-c", "team-d"})
	if want := []string{"team-a", "team-d"}; !slices.Equal(readable, want) {
		t.Errorf("readable = %q, want %q (an API-server blip says nothing about RBAC)", readable, want)
	}
	if blocked["team-b"] != ReasonForbidden || blocked["team-c"] != ReasonNamespaceNotFound || len(blocked) != 2 {
		t.Errorf("blocked = %v, want team-b forbidden and team-c namespace-not-found", blocked)
	}
}

// The prober reports the unreadable set every pass, and ends the manager
// (ErrSetChanged) when the readable set differs from the cached one, so a
// restored RoleBinding brings its namespace back.
func TestProberRestartsWhenTheSetChanges(t *testing.T) {
	var reports []map[string]string
	p := &Prober{
		Reader:   denying(nil),
		Watched:  []string{"team-a", "team-b"},
		Active:   []string{"team-a"},
		Interval: 10 * time.Millisecond,
		Report:   func(b map[string]string) { reports = append(reports, b) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Start(ctx); !errors.Is(err, ErrSetChanged) {
		t.Fatalf("Start = %v, want ErrSetChanged once team-b became readable", err)
	}
	if len(reports) == 0 || len(reports[0]) != 0 {
		t.Errorf("reports = %v, want an empty unreadable set reported", reports)
	}

	p = &Prober{Reader: denying(map[string]error{"team-b": forbidden}),
		Watched: []string{"team-a", "team-b"}, Active: []string{"team-a"}, Interval: 10 * time.Millisecond}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if err := p.Start(ctx2); err != nil {
		t.Errorf("Start = %v, want nil while the readable set is unchanged", err)
	}
}
