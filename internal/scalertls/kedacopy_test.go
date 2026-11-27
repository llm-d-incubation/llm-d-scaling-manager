package scalertls

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testKEDANS = "keda"

// kedaCopyPlaceholder is KEDA's copy as the release manifest pre-creates it:
// Opaque, labelled, no data.
func kedaCopyPlaceholder() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: KEDACopySecretName(testNS), Namespace: testKEDANS,
			Labels: map[string]string{OwnerLabel: "keda-client"}},
		Type: corev1.SecretTypeOpaque,
	}
}

// kedaStatus records the state and the KEDA connection Bootstrap published.
type kedaStatus struct {
	statusRecorder
	mu         sync.Mutex
	connection string
	stale      bool
}

func (k *kedaStatus) SetKEDAConnection(connection string, copyStale bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.connection, k.stale = connection, copyStale
}

func (k *kedaStatus) kedaGet() (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.connection, k.stale
}

func newCopyBootstrap(cl client.Client, st StatusSink) *Bootstrap {
	e := newEnsurer(cl, fixtureNow)
	return &Bootstrap{Ensurer: *e, Status: st, KEDA: &KEDACopy{Namespace: testKEDANS, SecretName: KEDACopySecretName(testNS)}}
}

// With the copy pre-created and no marker, the controller fills KEDA's copy
// from the client certificate it issued and serves mutual TLS from the
// start; KEDA presenting the certificate makes the connection current.
func TestBootstrap_KEDACopyWrittenByTheController(t *testing.T) {
	var c counter
	objs := append(placeholders(), kedaCopyPlaceholder())
	cl := newClient(t, &c, interceptor.Funcs{}, objs...)
	st := &kedaStatus{}
	b := newCopyBootstrap(cl, st)
	ca := prepare(t, b)
	if ca == nil {
		state, action := st.get()
		t.Fatalf("plaintext with KEDA's copy pre-created: %q\n%s", state, action)
	}
	src := getSecret(t, cl, ClientSecretName)
	var cp corev1.Secret
	if err := cl.Get(context.Background(), client2Key(testKEDANS, KEDACopySecretName(testNS)), &cp); err != nil {
		t.Fatal(err)
	}
	for _, k := range copyKeys {
		if len(cp.Data[k]) == 0 || !bytes.Equal(cp.Data[k], src.Data[k]) {
			t.Errorf("KEDA's copy %s differs from the client Secret's", k)
		}
	}
	if cp.Type != corev1.SecretTypeOpaque {
		t.Errorf("KEDA's copy is %s", cp.Type)
	}
	state, action := st.get()
	if !strings.HasPrefix(state, "mutual TLS") {
		t.Fatalf("state %q", state)
	}
	// A fresh CA: the only step is the conditional KEDA restart, for a
	// reinstall, never a copy command.
	if action != "" && !strings.HasPrefix(action, "1. only on a reinstall") {
		t.Fatalf("action %q", action)
	}
	if conn, stale := st.kedaGet(); conn != KEDAConnectionNone || stale {
		t.Fatalf("kedaConnection %q stale %v before any handshake", conn, stale)
	}

	clk := &clock{t: fixtureNow}
	addr := serve(t, b, ca, clk)
	if _, err := dial(t, cl, addr, fixtureNow); err != nil {
		t.Fatalf("KEDA with the copy cannot connect: %v", err)
	}
	if conn, _ := st.kedaGet(); conn != KEDAConnectionCurrent {
		t.Fatalf("kedaConnection %q after KEDA presented the current certificate", conn)
	}

	// The next pass clears the notice and does not write the copy again.
	rv := cp.ResourceVersion
	b.Check(context.Background())
	if _, action := st.get(); action != "" {
		t.Fatalf("action after KEDA presented the certificate: %q", action)
	}
	if err := cl.Get(context.Background(), client2Key(testKEDANS, KEDACopySecretName(testNS)), &cp); err != nil {
		t.Fatal(err)
	}
	if cp.ResourceVersion != rv {
		t.Fatalf("a pass with a current copy wrote it again (resourceVersion %s -> %s)", rv, cp.ResourceVersion)
	}
}

// Negative control: without the pre-created Secret the controller may not
// create one, so it serves plaintext, and the status says what is missing
// with no copy command for a person to run.
func TestBootstrap_KEDACopyMissingIsPlaintext(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, placeholders()...)
	st := &kedaStatus{}
	b := newCopyBootstrap(cl, st)
	if ca := prepare(t, b); ca != nil {
		t.Fatal("serves mutual TLS with no KEDA copy")
	}
	if c.creates != 0 {
		t.Fatalf("created %d objects; the controller may not create Secrets", c.creates)
	}
	state, action := st.get()
	if !strings.Contains(state, "could not write KEDA's copy") || !strings.Contains(state, "does not exist") {
		t.Fatalf("state %q", state)
	}
	if strings.Contains(action, "kubectl get secret") || !strings.Contains(action, "restart the controller") {
		t.Fatalf("action %q", action)
	}
	if _, stale := st.kedaGet(); !stale {
		t.Fatal("kedaCopyStale false with no copy")
	}
}

// A Secret under the copy's name without the owner label is not ours: it is
// never written.
func TestBootstrap_KEDACopyForeignIsNotWritten(t *testing.T) {
	var c counter
	foreign := kedaCopyPlaceholder()
	foreign.Labels = nil
	cl := newClient(t, &c, interceptor.Funcs{}, append(placeholders(), foreign)...)
	st := &kedaStatus{}
	b := newCopyBootstrap(cl, st)
	if ca := prepare(t, b); ca != nil {
		t.Fatal("serves mutual TLS over a foreign Secret")
	}
	var cp corev1.Secret
	if err := cl.Get(context.Background(), client2Key(testKEDANS, KEDACopySecretName(testNS)), &cp); err != nil {
		t.Fatal(err)
	}
	if len(cp.Data) != 0 {
		t.Fatal("wrote a Secret without the owner label")
	}
}

// Forbidden on the copy (no Role in KEDA's namespace, as when KEDA runs
// elsewhere and the manifest was not edited) is plaintext with the Role
// named, never a controller that does not start.
func TestBootstrap_KEDACopyForbiddenNamesTheRole(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Namespace == testKEDANS {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, nil)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, append(placeholders(), kedaCopyPlaceholder())...)
	st := &kedaStatus{}
	b := newCopyBootstrap(cl, st)
	if ca := prepare(t, b); ca != nil {
		t.Fatal("serves mutual TLS with the copy forbidden")
	}
	if state, _ := st.get(); !strings.Contains(state, KEDACopyRole) {
		t.Fatalf("state does not name Role %s: %q", KEDACopyRole, state)
	}
}

func client2Key(ns, name string) client.ObjectKey { return client.ObjectKey{Namespace: ns, Name: name} }
