package scalertls

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testNS = "team-a"

// fixtureNow is a day after testdata/installer was minted: every certificate
// in it is valid, the serving one for about 364 more days.
var fixtureNow = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

// counter counts the writes that reached the fake client.
type counter struct{ creates, updates int }

func (c *counter) total() int { return c.creates + c.updates }

// newClient returns a fake client holding objs, counting writes into c, with
// funcs (may be zero) intercepting before the count.
func newClient(t *testing.T, c *counter, funcs interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	create, update := funcs.Create, funcs.Update
	funcs.Create = func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if create != nil {
			if err := create(ctx, cl, obj, opts...); err != nil {
				return err
			}
		}
		c.creates++
		return cl.Create(ctx, obj, opts...)
	}
	funcs.Update = func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if update != nil {
			if err := update(ctx, cl, obj, opts...); err != nil {
				return err
			}
		}
		c.updates++
		return cl.Update(ctx, obj, opts...)
	}
	return interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), funcs)
}

func newEnsurer(cl client.Client, now time.Time) *Ensurer {
	return &Ensurer{Reader: cl, Writer: cl, Namespace: testNS, ClientSAN: "scaling-manager-keda-client",
		Now: func() time.Time { return now }}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "installer", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// installerSecret is Secret name as deploy/lib/scaler_tls.sh writes it
// (llmsm_scaler_write_secret), from the openssl-minted files of testdata/installer.
func installerSecret(t *testing.T, name, role, crt, key string) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{
			"app.kubernetes.io/managed-by": "llm-d-scaling-manager", OwnerLabel: role}},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:              readFixture(t, crt),
			corev1.TLSPrivateKeyKey:        readFixture(t, key),
			corev1.ServiceAccountRootCAKey: readFixture(t, "ca.crt"),
		},
	}
}

func installerSecrets(t *testing.T) []client.Object {
	return []client.Object{
		installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key"),
		installerSecret(t, ServingSecretName, "serving", "serving.crt", "serving.key"),
		installerSecret(t, ClientSecretName, "client", "client.crt", "client.key"),
	}
}

func getSecret(t *testing.T, cl client.Reader, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: name}, s); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return s
}

func certOf(t *testing.T, s *corev1.Secret) *x509.Certificate {
	t.Helper()
	c, err := firstCert(s.Data[corev1.TLSCertKey])
	if err != nil {
		t.Fatalf("%s: %v", s.Name, err)
	}
	return c
}

func ensure(t *testing.T, e *Ensurer) *Outcome {
	t.Helper()
	o, err := e.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return o
}

func TestEnsure_MintsTheInstallerShapeInAnEmptyNamespace(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{})
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if !o.CAMinted || !o.ServingIssued || !o.ClientIssued || !o.Usable() || len(o.Problems) > 0 {
		t.Fatalf("outcome: %+v", o)
	}
	if c.creates != 3 || c.updates != 0 {
		t.Fatalf("writes: %+v, want 3 creates", c)
	}

	ca := getSecret(t, cl, CASecretName)
	serving := getSecret(t, cl, ServingSecretName)
	clientS := getSecret(t, cl, ClientSecretName)
	for role, s := range map[string]*corev1.Secret{"ca": ca, "serving": serving, "client": clientS} {
		if s.Type != corev1.SecretTypeTLS {
			t.Errorf("%s: type %s", s.Name, s.Type)
		}
		if s.Labels[OwnerLabel] != role || s.Labels[managedByLabel] != managedByValue || s.Labels[partOfLabel] != partOfValue {
			t.Errorf("%s: labels %v", s.Name, s.Labels)
		}
		if !strings.HasPrefix(string(s.Data[corev1.TLSPrivateKeyKey]), "-----BEGIN PRIVATE KEY-----") {
			t.Errorf("%s: the key is not PKCS #8, as openssl 3 writes it", s.Name)
		}
		if string(s.Data[corev1.ServiceAccountRootCAKey]) != string(ca.Data[corev1.TLSCertKey]) {
			t.Errorf("%s: ca.crt is not the CA", s.Name)
		}
	}

	caCert := certOf(t, ca)
	wantCA := func(ok bool, what string) {
		if !ok {
			t.Errorf("CA: %s", what)
		}
	}
	wantCA(caCert.Subject.CommonName == "llmsm-scaler-ca."+testNS, "CN "+caCert.Subject.CommonName)
	wantCA(caCert.IsCA && caCert.BasicConstraintsValid, "not CA:TRUE")
	wantCA(caCert.KeyUsage == x509.KeyUsageCertSign|x509.KeyUsageCRLSign, "key usage")
	wantCA(len(caCert.SubjectKeyId) > 0, "no subject key identifier")
	wantCA(caCert.NotAfter.Sub(fixtureNow) == CAValidity, "validity")
	wantCA(caCert.PublicKey.(*rsa.PublicKey).N.BitLen() == 2048, "not RSA 2048")
	wantCA(caCert.SignatureAlgorithm == x509.SHA256WithRSA, "not sha256WithRSA")

	for _, tc := range []struct {
		s        *corev1.Secret
		cn       string
		dns      []string
		eku      x509.ExtKeyUsage
		validity time.Duration
	}{
		{serving, "llmsm-external-scaler.team-a.svc",
			[]string{"scaling-manager-external-scaler.team-a.svc", "scaling-manager-external-scaler.team-a.svc.cluster.local"},
			x509.ExtKeyUsageServerAuth, ServingValidity},
		{clientS, "scaling-manager-keda-client", []string{"scaling-manager-keda-client"}, x509.ExtKeyUsageClientAuth, ClientValidity},
	} {
		leaf := certOf(t, tc.s)
		if leaf.Subject.CommonName != tc.cn || !slices.Equal(leaf.DNSNames, tc.dns) {
			t.Errorf("%s: CN %q DNS %v", tc.s.Name, leaf.Subject.CommonName, leaf.DNSNames)
		}
		if leaf.IsCA || !leaf.BasicConstraintsValid || leaf.KeyUsage != x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment ||
			!slices.Equal(leaf.ExtKeyUsage, []x509.ExtKeyUsage{tc.eku}) {
			t.Errorf("%s: constraints or usages", tc.s.Name)
		}
		if len(leaf.SubjectKeyId) == 0 || !slices.Equal(leaf.AuthorityKeyId, caCert.SubjectKeyId) {
			t.Errorf("%s: no subject key identifier, or the authority key identifier is not the CA's", tc.s.Name)
		}
		if leaf.NotAfter.Sub(fixtureNow) != tc.validity || leaf.PublicKey.(*rsa.PublicKey).N.BitLen() != 2048 {
			t.Errorf("%s: validity %s or key size", tc.s.Name, leaf.NotAfter.Sub(fixtureNow))
		}
		roots := x509.NewCertPool()
		roots.AddCert(caCert)
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: fixtureNow, KeyUsages: []x509.ExtKeyUsage{tc.eku}}); err != nil {
			t.Errorf("%s does not chain to the CA: %v", tc.s.Name, err)
		}
	}

	// A second pass finds everything valid and writes nothing.
	c = counter{}
	o = ensure(t, newEnsurer(cl, fixtureNow.Add(time.Hour)))
	if c.total() != 0 || o.CAMinted || o.ServingIssued || o.ClientIssued || len(o.Adopted) != 3 {
		t.Fatalf("second pass wrote %+v, outcome %+v", c, o)
	}
}

// What make deploy-scaling-manager wrote (openssl, testdata/installer) is
// adopted as it is: no write, the same bytes served.
func TestEnsure_AdoptsTheInstallerSecretsUnchanged(t *testing.T) {
	var c counter
	objs := installerSecrets(t)
	cl := newClient(t, &c, interceptor.Funcs{}, objs...)
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if c.total() != 0 {
		t.Fatalf("wrote %+v to an install make made", c)
	}
	if o.CAMinted || o.ServingIssued || o.ClientIssued || len(o.Problems) > 0 || !o.Usable() || len(o.Adopted) != 3 {
		t.Fatalf("outcome: %+v", o)
	}
	if want := readFixture(t, "serving.crt"); string(o.Serving.certPEM) != string(want) {
		t.Fatal("does not serve the installer's serving certificate")
	}
	for _, obj := range objs {
		want := obj.(*corev1.Secret)
		got := getSecret(t, cl, want.Name)
		for k, v := range want.Data {
			if string(got.Data[k]) != string(v) {
				t.Errorf("%s/%s changed", want.Name, k)
			}
		}
	}
}

// A valid CA with no leaves: the leaves are issued from it, the CA is kept.
func TestEnsure_IssuesMissingLeavesFromTheExistingCA(t *testing.T) {
	var c counter
	ca := installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key")
	cl := newClient(t, &c, interceptor.Funcs{}, ca)
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if o.CAMinted || !o.ServingIssued || !o.ClientIssued || c.creates != 2 || c.updates != 0 {
		t.Fatalf("outcome %+v, writes %+v", o, c)
	}
	if string(getSecret(t, cl, CASecretName).Data[corev1.TLSCertKey]) != string(readFixture(t, "ca.crt")) {
		t.Fatal("the CA was replaced")
	}
	if Fingerprint(o.CA) != Fingerprint(certOf(t, ca)) {
		t.Fatal("the outcome's CA is not the existing one")
	}
}

// A CA Secret that exists is never replaced: not when it expires soon, not
// when it is expired, not when it holds garbage.
func TestEnsure_NeverReplacesAnExistingCA(t *testing.T) {
	caCert := func(t *testing.T) *x509.Certificate {
		return certOf(t, installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key"))
	}
	for _, tc := range []struct {
		name   string
		now    func(t *testing.T) time.Time
		mutate func(s *corev1.Secret)
		usable bool
		want   string
	}{
		{"expiring", func(t *testing.T) time.Time { return caCert(t).NotAfter.Add(-10 * 24 * time.Hour) }, nil, true, "never rotates it"},
		{"expired", func(t *testing.T) time.Time { return caCert(t).NotAfter.Add(time.Hour) }, nil, false, "never replaces an existing CA"},
		{"garbage", func(*testing.T) time.Time { return fixtureNow }, func(s *corev1.Secret) { s.Data[corev1.TLSCertKey] = []byte("junk") }, false, "never replaces an existing CA"},
		{"no key", func(*testing.T) time.Time { return fixtureNow }, func(s *corev1.Secret) { delete(s.Data, corev1.TLSPrivateKeyKey) }, false, "never replaces an existing CA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c counter
			ca := installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key")
			if tc.mutate != nil {
				tc.mutate(ca)
			}
			before := string(ca.Data[corev1.TLSCertKey])
			cl := newClient(t, &c, interceptor.Funcs{}, ca)
			o := ensure(t, newEnsurer(cl, tc.now(t)))
			if o.CAMinted {
				t.Fatal("minted a CA over an existing one")
			}
			if got := string(getSecret(t, cl, CASecretName).Data[corev1.TLSCertKey]); got != before {
				t.Fatal("the CA Secret changed")
			}
			if !tc.usable && (c.total() != 0 || o.Usable()) {
				t.Fatalf("an unusable CA: writes %+v, usable %v", c, o.Usable())
			}
			if !strings.Contains(strings.Join(o.Problems, " "), tc.want) ||
				!strings.Contains(strings.Join(o.Problems, " "), "kubectl delete secret "+CASecretName+" -n "+testNS) {
				t.Fatalf("problems %q do not say %q and how to rotate", o.Problems, tc.want)
			}
		})
	}
}

// A leaf the installer wrote is issued again when it expires within
// RenewBefore, names other hosts, or chains to another CA; the CA and the
// other leaf are kept.
func TestEnsure_ReissuesAnInvalidLeafAndKeepsTheRest(t *testing.T) {
	servingNotAfter := certOf(t, installerSecret(t, ServingSecretName, "serving", "serving.crt", "serving.key")).NotAfter
	for _, tc := range []struct {
		name string
		now  time.Time
		ns   string
	}{
		{"expiring", servingNotAfter.Add(-10 * 24 * time.Hour), testNS},
		{"other hosts", fixtureNow, "team-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c counter
			objs := installerSecrets(t)
			for _, o := range objs {
				o.SetNamespace(tc.ns)
			}
			cl := newClient(t, &c, interceptor.Funcs{}, objs...)
			e := newEnsurer(cl, tc.now)
			e.Namespace = tc.ns
			o := ensure(t, e)
			if o.CAMinted || !o.ServingIssued || o.ClientIssued || c.updates != 1 || c.creates != 0 {
				t.Fatalf("outcome %+v, writes %+v", o, c)
			}
			got := &corev1.Secret{}
			if err := cl.Get(context.Background(), client.ObjectKey{Namespace: tc.ns, Name: ServingSecretName}, got); err != nil {
				t.Fatal(err)
			}
			if got.Labels[OwnerLabel] != "serving" || got.Labels["app.kubernetes.io/managed-by"] != "llm-d-scaling-manager" {
				t.Fatalf("the update dropped the labels: %v", got.Labels)
			}
			if string(o.Serving.certPEM) != string(got.Data[corev1.TLSCertKey]) {
				t.Fatal("serves a certificate other than the one written")
			}
		})
	}

	t.Run("signed by another CA", func(t *testing.T) {
		var c counter
		other, err := mintCA(testNS, fixtureNow)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := issueLeaf(other, "scaling-manager-keda-client", []string{"scaling-manager-keda-client"},
			x509.ExtKeyUsageClientAuth, ClientValidity, fixtureNow)
		if err != nil {
			t.Fatal(err)
		}
		objs := installerSecrets(t)
		objs[2].(*corev1.Secret).Data = secretData(leaf, other.certPEM)
		cl := newClient(t, &c, interceptor.Funcs{}, objs...)
		o := ensure(t, newEnsurer(cl, fixtureNow))
		if !o.ClientIssued || o.ServingIssued || o.CAMinted || c.updates != 1 {
			t.Fatalf("outcome %+v, writes %+v", o, c)
		}
	})
}

// cert-manager's Secrets are never written: not when they are valid, not
// when one is missing, and the CA is not minted beside them.
func TestEnsure_LeavesCertManagerSecretsAlone(t *testing.T) {
	certManaged := func(s *corev1.Secret, how string) {
		switch how {
		case "annotation":
			s.Annotations = map[string]string{"cert-manager.io/certificate-name": s.Name}
			delete(s.Labels, OwnerLabel)
		case "owner":
			s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "cert-manager.io/v1", Kind: "Certificate", Name: s.Name, UID: "u"}}
			delete(s.Labels, OwnerLabel)
		case "label":
			s.Labels[OwnerLabel] = "cert-manager"
		}
	}
	for _, how := range []string{"annotation", "owner", "label"} {
		t.Run(how+"/all valid", func(t *testing.T) {
			var c counter
			objs := installerSecrets(t)
			for _, o := range objs {
				certManaged(o.(*corev1.Secret), how)
			}
			cl := newClient(t, &c, interceptor.Funcs{}, objs...)
			// Late enough that the installer's own leaves would be re-issued.
			now := certOf(t, objs[1].(*corev1.Secret)).NotAfter.Add(-10 * 24 * time.Hour)
			o := ensure(t, newEnsurer(cl, now))
			if c.total() != 0 {
				t.Fatalf("wrote %+v to cert-manager's Secrets", c)
			}
			if !o.Usable() || string(o.Serving.certPEM) != string(readFixture(t, "serving.crt")) {
				t.Fatalf("does not serve cert-manager's certificate: %+v", o)
			}
		})
		t.Run(how+"/CA and serving missing", func(t *testing.T) {
			var c counter
			cs := installerSecret(t, ClientSecretName, "client", "client.crt", "client.key")
			certManaged(cs, how)
			cl := newClient(t, &c, interceptor.Funcs{}, cs)
			o := ensure(t, newEnsurer(cl, fixtureNow))
			if c.total() != 0 || o.CAMinted || o.Usable() {
				t.Fatalf("writes %+v, outcome %+v", c, o)
			}
			if !strings.Contains(strings.Join(o.Problems, " "), "managed by cert-manager") {
				t.Fatalf("problems: %q", o.Problems)
			}
		})
	}
}

// A Secret under one of the names that nobody marked as the scaling
// manager's is never overwritten, even when it is unusable.
func TestEnsure_NeverOverwritesAnUnlabelledLeaf(t *testing.T) {
	var c counter
	objs := installerSecrets(t)
	serving := objs[1].(*corev1.Secret)
	delete(serving.Labels, OwnerLabel)
	serving.Data[corev1.TLSCertKey] = []byte("junk")
	cl := newClient(t, &c, interceptor.Funcs{}, objs...)
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if c.total() != 0 || o.Serving != nil {
		t.Fatalf("writes %+v, serving %v", c, o.Serving != nil)
	}
	if !strings.Contains(strings.Join(o.Problems, " "), "does not overwrite it") {
		t.Fatalf("problems: %q", o.Problems)
	}
}

// Forbidden is not absent: a Secret that cannot be read is never minted over.
func TestEnsure_AReadErrorWritesNothing(t *testing.T) {
	for _, err := range []error{
		apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, ServingSecretName, nil),
		apierrors.NewServiceUnavailable("down"),
	} {
		var c counter
		cl := newClient(t, &c, interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == ServingSecretName {
					return err
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		})
		o, got := newEnsurer(cl, fixtureNow).Ensure(context.Background())
		if got == nil || o != nil || c.total() != 0 {
			t.Fatalf("%v: err %v, outcome %+v, writes %+v", err, got, o, c)
		}
	}
}

// Two replicas start at once: the one whose Create of the CA loses adopts the
// winner's CA instead of failing or minting another.
func TestEnsure_ALostRaceAdoptsTheWinnersCA(t *testing.T) {
	var c counter
	winner, err := mintCA(testNS, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	raced := false
	cl := newClient(t, &c, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == CASecretName && !raced {
				raced = true
				s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: CASecretName, Namespace: testNS,
					Labels: map[string]string{OwnerLabel: "ca"}}, Type: corev1.SecretTypeTLS, Data: secretData(winner, winner.certPEM)}
				if err := cl.Create(ctx, s); err != nil {
					t.Fatal(err)
				}
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, CASecretName)
			}
			return nil
		},
	})
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if o.CAMinted || Fingerprint(o.CA) != Fingerprint(winner.cert) || !o.Usable() {
		t.Fatalf("outcome %+v", o)
	}
}
