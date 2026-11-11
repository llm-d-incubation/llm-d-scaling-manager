package scalertls

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

// placeholder is Secret name as the release file scaling-manager-scaler-tls.yaml
// creates it: empty, of type kubernetes.io/tls, with the installer label.
func placeholder(name, role string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{
			"app.kubernetes.io/managed-by": "llm-d-scaling-manager", "app.kubernetes.io/part-of": "scaling-manager", OwnerLabel: role}},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: {}, corev1.TLSPrivateKeyKey: {}, corev1.ServiceAccountRootCAKey: {}},
	}
}

func placeholders() []client.Object {
	return []client.Object{
		placeholder(CASecretName, "ca"), placeholder(ServingSecretName, "serving"), placeholder(ClientSecretName, "client"),
	}
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

// fixtureCA is the installer-minted CA as a key pair, to sign test leaves.
func fixtureCA(t *testing.T) *keyPair {
	t.Helper()
	ca, err := parsePair(readFixture(t, "ca.crt"), readFixture(t, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	return ca
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

func problems(o *Outcome) string { return strings.Join(o.Problems, " ") }

// installerDays reads LLMSM_SCALER_<name>_DAYS from deploy/lib/scaler_tls.sh,
// following one level of $LLMSM_SCALER_<other>_DAYS, so the lifetimes here
// cannot drift from the installer's.
func installerDays(t *testing.T, name string) time.Duration {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "lib", "scaler_tls.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^LLMSM_SCALER_` + name + `_DAYS=(\S+)$`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("LLMSM_SCALER_%s_DAYS not found in deploy/lib/scaler_tls.sh", name)
	}
	v := string(m[1])
	if ref := regexp.MustCompile(`^\$LLMSM_SCALER_(\w+)_DAYS$`).FindStringSubmatch(v); ref != nil {
		return installerDays(t, ref[1])
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("LLMSM_SCALER_%s_DAYS=%q", name, v)
	}
	return time.Duration(n) * 24 * time.Hour
}

// The lifetimes and the renewal window are the installer's, read from its
// script, and the values the proposal names.
func TestLifetimesAreTheInstallers(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  time.Duration
		days int
	}{
		{"CA", CAValidity, 3650}, {"LEAF", ServingValidity, 365}, {"CLIENT", ClientValidity, 3650}, {"RENEW", RenewBefore, 30},
	} {
		want := installerDays(t, tc.name)
		if tc.got != want || want != time.Duration(tc.days)*24*time.Hour {
			t.Errorf("%s: here %s, installer %s, want %d days", tc.name, tc.got, want, tc.days)
		}
	}
}

func TestEnsure_MintsTheInstallerShapeIntoThePlaceholders(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, placeholders()...)
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if !o.CAMinted || !o.ServingIssued || !o.ClientIssued || !o.Usable() || len(o.Problems) > 0 {
		t.Fatalf("outcome: %+v", o)
	}
	if c.creates != 0 || c.updates != 3 {
		t.Fatalf("writes: %+v, want 3 updates and no create", c)
	}
	if o.Pending != PendingCA || getSecret(t, cl, ClientSecretName).Annotations[PendingAnnotation] != PendingCA {
		t.Fatalf("pending %q: KEDA's copy is due, for a new CA", o.Pending)
	}

	ca := getSecret(t, cl, CASecretName)
	serving := getSecret(t, cl, ServingSecretName)
	clientS := getSecret(t, cl, ClientSecretName)
	for role, s := range map[string]*corev1.Secret{"ca": ca, "serving": serving, "client": clientS} {
		if s.Type != corev1.SecretTypeTLS || s.Labels[OwnerLabel] != role || s.Labels["app.kubernetes.io/part-of"] != "scaling-manager" {
			t.Errorf("%s: type %s, labels %v", s.Name, s.Type, s.Labels)
		}
		if !strings.HasPrefix(string(s.Data[corev1.TLSPrivateKeyKey]), "-----BEGIN PRIVATE KEY-----") {
			t.Errorf("%s: the key is not PKCS #8, as openssl 3 writes it", s.Name)
		}
		if string(s.Data[corev1.ServiceAccountRootCAKey]) != string(ca.Data[corev1.TLSCertKey]) {
			t.Errorf("%s: ca.crt is not the CA", s.Name)
		}
	}

	caCert := certOf(t, ca)
	want := func(ok bool, what string) {
		if !ok {
			t.Errorf("CA: %s", what)
		}
	}
	want(caCert.Subject.CommonName == "llmsm-scaler-ca."+testNS, "CN "+caCert.Subject.CommonName)
	want(caCert.IsCA && caCert.BasicConstraintsValid, "not CA:TRUE")
	want(caCert.KeyUsage == x509.KeyUsageCertSign|x509.KeyUsageCRLSign, "key usage")
	want(len(caCert.SubjectKeyId) > 0, "no subject key identifier")
	want(caCert.NotAfter.Sub(fixtureNow) == installerDays(t, "CA"), "validity")
	want(caCert.PublicKey.(*rsa.PublicKey).N.BitLen() == 2048, "not RSA 2048")
	want(caCert.SignatureAlgorithm == x509.SHA256WithRSA, "not sha256WithRSA")

	for _, tc := range []struct {
		s        *corev1.Secret
		cn       string
		dns      []string
		eku      x509.ExtKeyUsage
		validity time.Duration
	}{
		{serving, "llmsm-external-scaler.team-a.svc",
			[]string{"scaling-manager-external-scaler.team-a.svc", "scaling-manager-external-scaler.team-a.svc.cluster.local"},
			x509.ExtKeyUsageServerAuth, installerDays(t, "LEAF")},
		{clientS, "scaling-manager-keda-client", []string{"scaling-manager-keda-client"}, x509.ExtKeyUsageClientAuth, installerDays(t, "CLIENT")},
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
		if !chains(leaf, caCert, tc.eku, fixtureNow) {
			t.Errorf("%s does not chain to the CA", tc.s.Name)
		}
	}
	serials := map[string]bool{}
	for _, s := range []*corev1.Secret{ca, serving, clientS} {
		sn := certOf(t, s).SerialNumber
		if sn.BitLen() < 64 || serials[sn.String()] {
			t.Errorf("%s: serial %s is short or repeated", s.Name, sn)
		}
		serials[sn.String()] = true
	}

	// A second pass finds everything valid and writes nothing.
	c = counter{}
	o = ensure(t, newEnsurer(cl, fixtureNow.Add(time.Hour)))
	if c.total() != 0 || o.CAMinted || o.ServingIssued || o.ClientIssued || len(o.Adopted) != 3 || o.Pending != PendingCA {
		t.Fatalf("second pass wrote %+v, outcome %+v", c, o)
	}
}

// Without the placeholders there is nothing to fill: the controller may not
// create Secrets, so it says how to create them and writes nothing.
func TestEnsure_WithoutThePlaceholdersSaysHowToCreateThem(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{})
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if c.total() != 0 || o.Usable() || o.CAMinted {
		t.Fatalf("writes %+v, outcome %+v", c, o)
	}
	if !strings.Contains(problems(o), "kubectl create secret generic "+CASecretName+" -n "+testNS+" --type=kubernetes.io/tls") {
		t.Fatalf("problems: %q", o.Problems)
	}

	// The CA but no leaf placeholders: each missing one is named.
	c = counter{}
	cl = newClient(t, &c, interceptor.Funcs{}, installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key"))
	o = ensure(t, newEnsurer(cl, fixtureNow))
	if c.total() != 0 || o.Usable() || !strings.Contains(problems(o), "Secret "+ServingSecretName+" does not exist") ||
		!strings.Contains(problems(o), "Secret "+ClientSecretName+" does not exist") {
		t.Fatalf("writes %+v, problems %q", c, o.Problems)
	}
}

// What make deploy-scaling-manager wrote (openssl, testdata/installer) is
// adopted as it is: no write, the same bytes served, nothing pending.
func TestEnsure_AdoptsTheInstallerSecretsUnchanged(t *testing.T) {
	var c counter
	objs := installerSecrets(t)
	cl := newClient(t, &c, interceptor.Funcs{}, objs...)
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if c.total() != 0 {
		t.Fatalf("wrote %+v to an install make made", c)
	}
	if o.CAMinted || o.ServingIssued || o.ClientIssued || len(o.Problems) > 0 || !o.Usable() || len(o.Adopted) != 3 || o.Pending != "" {
		t.Fatalf("outcome: %+v", o)
	}
	if want := readFixture(t, "serving.crt"); string(o.Serving.certPEM) != string(want) || o.TrustFrom != TrustCA {
		t.Fatal("does not serve the installer's serving certificate, trusted through the CA")
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

// A valid CA with empty leaf placeholders: the leaves are issued from it, the
// CA is kept, and KEDA's copy is pending for a new client only.
func TestEnsure_IssuesEmptyLeavesFromTheExistingCA(t *testing.T) {
	var c counter
	ca := installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key")
	cl := newClient(t, &c, interceptor.Funcs{}, ca, placeholder(ServingSecretName, "serving"), placeholder(ClientSecretName, "client"))
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if o.CAMinted || !o.ServingIssued || !o.ClientIssued || c.creates != 0 || c.updates != 2 {
		t.Fatalf("outcome %+v, writes %+v", o, c)
	}
	if string(getSecret(t, cl, CASecretName).Data[corev1.TLSCertKey]) != string(readFixture(t, "ca.crt")) {
		t.Fatal("the CA was replaced")
	}
	if Fingerprint(o.CA) != Fingerprint(certOf(t, ca)) {
		t.Fatal("the outcome's CA is not the existing one")
	}
	// The empty client placeholder held no certificate KEDA could have: as
	// for a new CA, KEDA's copy is new.
	if o.Pending != PendingCA {
		t.Fatalf("pending %q", o.Pending)
	}
}

// A CA Secret that holds anything is never replaced: not when it expires
// soon, not when it is expired, not when it holds garbage or a certificate
// that is no CA.
func TestEnsure_NeverReplacesAnExistingCA(t *testing.T) {
	caCert := func(t *testing.T) *x509.Certificate {
		return certOf(t, installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key"))
	}
	for _, tc := range []struct {
		name   string
		now    func(t *testing.T) time.Time
		mutate func(t *testing.T, s *corev1.Secret)
		usable bool
		want   string
	}{
		{"expiring", func(t *testing.T) time.Time { return caCert(t).NotAfter.Add(-10 * 24 * time.Hour) }, nil, true, "never rotates it"},
		{"expired", func(t *testing.T) time.Time { return caCert(t).NotAfter.Add(time.Hour) }, nil, false, "never replaces an existing CA"},
		{"garbage", func(*testing.T) time.Time { return fixtureNow },
			func(_ *testing.T, s *corev1.Secret) { s.Data[corev1.TLSCertKey] = []byte("junk") }, false, "never replaces an existing CA"},
		{"no key", func(*testing.T) time.Time { return fixtureNow },
			func(_ *testing.T, s *corev1.Secret) { delete(s.Data, corev1.TLSPrivateKeyKey) }, false, "never replaces an existing CA"},
		{"not a CA", func(*testing.T) time.Time { return fixtureNow }, func(t *testing.T, s *corev1.Secret) {
			s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey] = readFixture(t, "serving.crt"), readFixture(t, "serving.key")
		}, false, "is not a CA certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c counter
			objs := installerSecrets(t)
			ca := objs[0].(*corev1.Secret)
			if tc.mutate != nil {
				tc.mutate(t, ca)
			}
			before := string(ca.Data[corev1.TLSCertKey])
			cl := newClient(t, &c, interceptor.Funcs{}, objs...)
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
			if !strings.Contains(problems(o), tc.want) || !strings.Contains(problems(o), "kubectl patch secret "+CASecretName+" -n "+testNS) {
				t.Fatalf("problems %q do not say %q and how to rotate", o.Problems, tc.want)
			}
		})
	}
}

// A leaf the installer wrote is issued again when it expires within
// RenewBefore, names other hosts, chains to another CA, has the wrong usage,
// or (the client) carries another ca.crt; the CA and the other leaf are kept.
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

	t.Run("client signed by another CA", func(t *testing.T) {
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
		if !o.ClientIssued || o.ServingIssued || o.CAMinted || c.updates != 1 || o.Pending != PendingCA {
			t.Fatalf("outcome %+v, writes %+v", o, c)
		}
	})

	t.Run("client with another ca.crt", func(t *testing.T) {
		var c counter
		other, err := mintCA(testNS, fixtureNow)
		if err != nil {
			t.Fatal(err)
		}
		objs := installerSecrets(t)
		objs[2].(*corev1.Secret).Data[corev1.ServiceAccountRootCAKey] = other.certPEM
		cl := newClient(t, &c, interceptor.Funcs{}, objs...)
		o := ensure(t, newEnsurer(cl, fixtureNow))
		if !o.ClientIssued || c.updates != 1 || o.Pending != PendingClient {
			t.Fatalf("a client Secret whose ca.crt is not the CA was kept: outcome %+v, writes %+v", o, c)
		}
		if !sameCert(getSecret(t, cl, ClientSecretName).Data[corev1.ServiceAccountRootCAKey], certOf(t, objs[0].(*corev1.Secret))) {
			t.Fatal("the re-issued client Secret does not carry the CA as ca.crt")
		}
	})

	t.Run("serving with client auth only", func(t *testing.T) {
		var c counter
		leaf, err := issueLeaf(fixtureCA(t), "llmsm-external-scaler.team-a.svc",
			[]string{"scaling-manager-external-scaler.team-a.svc", "scaling-manager-external-scaler.team-a.svc.cluster.local"},
			x509.ExtKeyUsageClientAuth, ServingValidity, fixtureNow)
		if err != nil {
			t.Fatal(err)
		}
		objs := installerSecrets(t)
		objs[1].(*corev1.Secret).Data = secretData(leaf, readFixture(t, "ca.crt"))
		cl := newClient(t, &c, interceptor.Funcs{}, objs...)
		o := ensure(t, newEnsurer(cl, fixtureNow))
		if !o.ServingIssued || c.updates != 1 || !slices.Equal(o.Serving.cert.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
			t.Fatalf("a serving leaf without server auth was kept: outcome %+v", o)
		}
	})
}

// cert-manager's Secrets are never written: not when they are valid, not
// when one is missing, and nothing is served that does not verify.
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
	managed := func(t *testing.T, how string) []client.Object {
		objs := installerSecrets(t)
		for _, o := range objs {
			certManaged(o.(*corev1.Secret), how)
		}
		return objs
	}
	servingNotAfter := certOf(t, installerSecret(t, ServingSecretName, "serving", "serving.crt", "serving.key")).NotAfter
	caNotAfter := certOf(t, installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key")).NotAfter
	for _, how := range []string{"annotation", "owner", "label"} {
		t.Run(how+"/all valid", func(t *testing.T) {
			var c counter
			cl := newClient(t, &c, interceptor.Funcs{}, managed(t, how)...)
			// Late enough that the installer's own leaves would be re-issued.
			o := ensure(t, newEnsurer(cl, servingNotAfter.Add(-10*24*time.Hour)))
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
			cl := newClient(t, &c, interceptor.Funcs{}, cs, placeholder(CASecretName, "ca"))
			o := ensure(t, newEnsurer(cl, fixtureNow))
			if c.total() != 0 || o.CAMinted || o.Usable() {
				t.Fatalf("writes %+v, outcome %+v", c, o)
			}
			if !strings.Contains(problems(o), "managed by cert-manager") {
				t.Fatalf("problems: %q", o.Problems)
			}
		})
	}
	for _, tc := range []struct {
		name, want string
		now        time.Time
	}{
		{"serving expired", "Secret " + ServingSecretName + " is not usable", servingNotAfter.Add(time.Hour)},
		{"CA expired", "Secret " + CASecretName + " holds no usable CA certificate (expired", caNotAfter.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c counter
			cl := newClient(t, &c, interceptor.Funcs{}, managed(t, "annotation")...)
			o := ensure(t, newEnsurer(cl, tc.now))
			if c.total() != 0 || o.Serving != nil || o.Usable() || !strings.Contains(problems(o), "expired") ||
				!strings.Contains(problems(o), tc.want) {
				t.Fatalf("serves an expired certificate of cert-manager's: writes %+v, problems %q", c, o.Problems)
			}
		})
	}
}

// serviceCASecrets is the installer's OpenShift layout: the CA and client
// from openssl, the serving Secret from the OpenShift service CA, whose
// bundle is in ConfigMap ServiceCAConfigMap.
func serviceCASecrets(t *testing.T, withBundle bool) []client.Object {
	t.Helper()
	svcCA, err := mintCA("openshift-service-ca", fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := issueLeaf(svcCA, "scaling-manager-external-scaler.team-a.svc",
		[]string{"scaling-manager-external-scaler.team-a.svc", "scaling-manager-external-scaler.team-a.svc.cluster.local"},
		x509.ExtKeyUsageServerAuth, ServingValidity, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	objs := installerSecrets(t)
	serving := objs[1].(*corev1.Secret)
	delete(serving.Labels, OwnerLabel)
	serving.Annotations = map[string]string{openShiftServingAnnotation: ServiceName}
	serving.Data = map[string][]byte{corev1.TLSCertKey: leaf.certPEM, corev1.TLSPrivateKeyKey: leaf.keyPEM}
	if withBundle {
		objs = append(objs, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ServiceCAConfigMap, Namespace: testNS},
			Data: map[string]string{ServiceCAKey: string(svcCA.certPEM)}})
	}
	return objs
}

// A serving certificate the OpenShift service CA issued is served as it is,
// verified against the service CA bundle, which is what KEDA must trust.
func TestEnsure_ServesTheServiceCAsCertificateAgainstItsBundle(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, serviceCASecrets(t, true)...)
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if c.total() != 0 || !o.Usable() || o.TrustFrom != TrustServiceCA || len(o.Problems) > 0 {
		t.Fatalf("writes %+v, outcome %+v", c, o)
	}
	if Fingerprint(o.CA) != Fingerprint(certOf(t, installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key"))) {
		t.Fatal("clients must still be verified with the scaler CA")
	}

	c = counter{}
	cl = newClient(t, &c, interceptor.Funcs{}, serviceCASecrets(t, false)...)
	o = ensure(t, newEnsurer(cl, fixtureNow))
	if c.total() != 0 || o.Serving != nil || !strings.Contains(problems(o), ServiceCAConfigMap) {
		t.Fatalf("served the service CA's certificate unverified: writes %+v, problems %q", c, o.Problems)
	}
}

// An unlabelled serving Secret is served only when it verifies against the
// CA KEDA trusts, and never overwritten.
func TestEnsure_AnUnlabelledLeafIsVerifiedAndNeverOverwritten(t *testing.T) {
	var c counter
	objs := installerSecrets(t)
	delete(objs[1].(*corev1.Secret).Labels, OwnerLabel)
	cl := newClient(t, &c, interceptor.Funcs{}, objs...)
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if c.total() != 0 || o.Serving == nil || o.TrustFrom != TrustCA {
		t.Fatalf("a valid unlabelled serving Secret: writes %+v, outcome %+v", c, o)
	}

	other, err := mintCA(testNS, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := issueLeaf(other, "x", []string{"scaling-manager-external-scaler.team-a.svc"}, x509.ExtKeyUsageServerAuth, ServingValidity, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]map[string][]byte{
		"junk":         {corev1.TLSCertKey: []byte("junk"), corev1.TLSPrivateKeyKey: readFixture(t, "serving.key")},
		"another CA's": secretData(leaf, other.certPEM),
	} {
		c = counter{}
		objs := installerSecrets(t)
		serving := objs[1].(*corev1.Secret)
		delete(serving.Labels, OwnerLabel)
		serving.Data = data
		cl := newClient(t, &c, interceptor.Funcs{}, objs...)
		o := ensure(t, newEnsurer(cl, fixtureNow))
		if c.total() != 0 || o.Serving != nil || !strings.Contains(problems(o), "does not overwrite it") {
			t.Fatalf("%s: writes %+v, serving %v, problems %q", name, c, o.Serving != nil, o.Problems)
		}
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
		}, placeholders()...)
		o, got := newEnsurer(cl, fixtureNow).Ensure(context.Background())
		if got == nil || o != nil || c.total() != 0 {
			t.Fatalf("%v: err %v, outcome %+v, writes %+v", err, got, o, c)
		}
	}
}

// Two replicas fill the placeholders at once: the one whose Update of the CA
// conflicts adopts the winner's CA instead of failing or minting over it.
func TestEnsure_ALostRaceAdoptsTheWinnersCA(t *testing.T) {
	var c counter
	winner, err := mintCA(testNS, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	raced := false
	cl := newClient(t, &c, interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == CASecretName && !raced {
				raced = true
				s := placeholder(CASecretName, "ca")
				cur := &corev1.Secret{}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(s), cur); err != nil {
					t.Fatal(err)
				}
				cur.Data = secretData(winner, winner.certPEM)
				if err := cl.Update(ctx, cur); err != nil {
					t.Fatal(err)
				}
				return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, CASecretName, nil)
			}
			return nil
		},
	}, placeholders()...)
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if o.CAMinted || Fingerprint(o.CA) != Fingerprint(winner.cert) || !o.Usable() {
		t.Fatalf("outcome %+v", o)
	}
}

// A Secret deleted between the read and the write is read again, not an
// error: the next pass finds it absent and says so.
func TestEnsure_ASecretDeletedUnderTheWriteIsReadAgain(t *testing.T) {
	var c counter
	deleted := false
	cl := newClient(t, &c, interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == ServingSecretName && !deleted {
				deleted = true
				if err := cl.Delete(ctx, obj); err != nil {
					t.Fatal(err)
				}
				return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, ServingSecretName)
			}
			return nil
		},
	}, installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key"), placeholder(ServingSecretName, "serving"),
		installerSecret(t, ClientSecretName, "client", "client.crt", "client.key"))
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if o.Serving != nil || !strings.Contains(problems(o), "Secret "+ServingSecretName+" does not exist") {
		t.Fatalf("outcome %+v", o)
	}
}

// The pending KEDA copy is cleared only for the certificate KEDA presented.
func TestEnsure_ClearPendingOnlyForTheCertificateInTheSecret(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, placeholders()...)
	e := newEnsurer(cl, fixtureNow)
	o := ensure(t, e)
	if err := e.ClearPending(context.Background(), "not-it"); err != nil {
		t.Fatal(err)
	}
	if getSecret(t, cl, ClientSecretName).Annotations[PendingAnnotation] != PendingCA {
		t.Fatal("cleared for another certificate")
	}
	if err := e.ClearPending(context.Background(), Fingerprint(o.Client)); err != nil {
		t.Fatal(err)
	}
	if _, ok := getSecret(t, cl, ClientSecretName).Annotations[PendingAnnotation]; ok {
		t.Fatal("not cleared for the certificate in the Secret")
	}
	if o := ensure(t, e); o.Pending != "" {
		t.Fatalf("pending %q after clearing", o.Pending)
	}
}

// A CA Secret whose tls.crt also holds the key never passes the key on: the
// ca.crt written into a leaf Secret, which KEDA's copy carries, is the
// certificate alone.
func TestEnsure_NoKeyMaterialIntoTheLeaves(t *testing.T) {
	var c counter
	ca := installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key")
	ca.Data[corev1.TLSCertKey] = append(append([]byte{}, ca.Data[corev1.TLSCertKey]...), ca.Data[corev1.TLSPrivateKeyKey]...)
	cl := newClient(t, &c, interceptor.Funcs{}, ca, placeholder(ServingSecretName, "serving"), placeholder(ClientSecretName, "client"))
	o := ensure(t, newEnsurer(cl, fixtureNow))
	if !o.ServingIssued || !o.ClientIssued {
		t.Fatalf("outcome %+v", o)
	}
	for _, name := range []string{ServingSecretName, ClientSecretName} {
		if got := string(getSecret(t, cl, name).Data[corev1.ServiceAccountRootCAKey]); strings.Contains(got, "PRIVATE KEY") || got != string(pemOf(o.CA)) {
			t.Fatalf("%s: ca.crt is not the CA certificate alone:\n%s", name, got)
		}
	}
	if strings.Contains(string(o.Trust), "PRIVATE KEY") {
		t.Fatal("the trust handed on holds key material")
	}
}
