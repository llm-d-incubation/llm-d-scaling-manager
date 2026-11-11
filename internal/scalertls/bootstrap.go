package scalertls

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultInterval is how often Bootstrap checks the Secrets after the first
// pass: a serving certificate is issued again at most this long after it
// enters RenewBefore, and an emptied CA is minted again within it.
const DefaultInterval = 10 * time.Minute

// ControllerDeployment is the controller's Deployment, which every install
// path names the same.
const ControllerDeployment = "scaling-manager-controller-manager"

// prepareRetry is the pause between two attempts of Prepare.
var prepareRetry = 2 * time.Second

// StatusSink receives the state of the scaler's TLS for the controller-status
// ConfigMap: state is always set; action is what a person must run, or "".
// SetScalerCA receives the public PEM the serving certificate verifies
// against (certificates only, never a key) and the sha256 of the first, or
// "" for both when there is none.
type StatusSink interface {
	SetScalerTLS(state, action string)
	SetScalerCA(certPEM, sha256Hex string)
}

// markerState is what the KEDA marker ConfigMap says.
type markerState int

const (
	// markerUnknown: it could not be read; neither present nor absent.
	markerUnknown markerState = iota
	markerAbsent
	// markerForeign: it exists without OwnerLabel, so it is not KEDA's side
	// as the release file or the installer writes it.
	markerForeign
	markerPresent
)

// Bootstrap provisions the scaler's certificates (Ensurer) at start and every
// Interval, serves the current serving certificate, and reports what an
// operator must do. It decides once, in Prepare, whether the scaler serves
// mutual TLS: when the certificates are usable and KEDA's side is in place
// (ConfigMap MarkerConfigMapName exists), as the installer decides; plaintext
// otherwise.
//
// After Prepare, a renewed serving certificate is served from the next
// handshake on, with no restart, as long as it chains to the CA the scaler
// verifies clients with, which is fixed for the life of the process. A change
// of CA is reported, never served.
type Bootstrap struct {
	Ensurer
	// Status receives the state; nil reports nothing.
	Status StatusSink
	// Interval is the period of the checks after Prepare; 0 is DefaultInterval.
	Interval time.Duration
	// Log is the logger; the zero value logs under ctrl.Log.
	Log logr.Logger

	serving atomic.Pointer[tls.Certificate]

	mu sync.Mutex
	// tlsOn: Prepare chose mutual TLS.
	tlsOn bool
	// clientCA is the CA the scaler verifies clients with, when tlsOn.
	clientCA *x509.Certificate
	// servedTrust is what the served certificate verifies against, when
	// tlsOn: the Trust of the outcome it came from.
	servedTrust []byte
	// presented holds the fingerprints of client certificates accepted in a
	// handshake since the last check.
	presented map[string]bool
	// last is the latest successful outcome, lastErr the latest pass's error.
	last    *Outcome
	lastErr error
	// markerAtStart and markerNow: KEDA's side at Prepare, and at the latest
	// pass; markerErr why the latest read failed.
	markerAtStart, markerNow markerState
	markerErr                error
}

func (b *Bootstrap) logger() logr.Logger {
	if b.Log.GetSink() == nil {
		return ctrl.Log.WithName("scaler-tls")
	}
	return b.Log
}

// Prepare runs the first pass and decides whether the scaler serves mutual
// TLS. It returns the CA client certificates must chain to when it does, nil
// when the scaler serves plaintext.
//
// It fails closed. It retries, while ctx lasts, a marker it cannot read and,
// when the marker is present, a pass that fails; if either is still unknown
// when ctx ends it returns an error and the controller does not start.
// Choosing plaintext then would leave KEDA dialling TLS to a scaler that
// never answers it, for the life of the process, with nothing scaling.
func (b *Bootstrap) Prepare(ctx context.Context) (*x509.Certificate, error) {
	var (
		o      *Outcome
		err    error
		marker markerState
		merr   error
	)
	for {
		marker, merr = b.readMarker(ctx)
		o, err = b.Ensure(ctx)
		settled := marker != markerUnknown && (err == nil || marker != markerPresent)
		if settled || ctx.Err() != nil {
			break
		}
		b.logger().Info("Scaler TLS: cannot tell yet how to serve, retrying",
			"markerError", errText(merr), "error", errText(err))
		select {
		case <-ctx.Done():
		case <-time.After(prepareRetry):
		}
	}

	b.mu.Lock()
	b.markerAtStart, b.markerNow, b.markerErr = marker, marker, merr
	b.record(o, err)
	if err == nil && o.Usable() && marker == markerPresent {
		b.tlsOn = true
		b.clientCA = o.CA
		b.servedTrust = o.Trust
		b.serving.Store(o.Serving.tlsCertificate())
	}
	ca := b.clientCA
	b.mu.Unlock()
	b.publish()

	switch {
	case marker == markerUnknown:
		return nil, fmt.Errorf("cannot tell whether KEDA's side of mutual TLS is in place, so the scaler would serve "+
			"the wrong protocol: %w", merr)
	case marker == markerPresent && err != nil:
		return nil, fmt.Errorf("KEDA's side of mutual TLS is in place, but the scaler TLS Secrets cannot be read or "+
			"written: %w", err)
	}
	return ca, nil
}

// errText is err's message, "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Start checks the Secrets every Interval until ctx ends. It implements
// manager.Runnable.
func (b *Bootstrap) Start(ctx context.Context) error {
	interval := b.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			b.Check(ctx)
		}
	}
}

// NeedLeaderElection is false: every replica serves the scaler and needs the
// certificate. Writes race safely: an Update that conflicts re-reads and
// adopts what the other replica wrote.
func (b *Bootstrap) NeedLeaderElection() bool { return false }

// Check runs one pass: it renews what the rules allow, serves a renewed
// serving certificate, clears the pending KEDA copy once KEDA presented the
// certificate, and reports.
func (b *Bootstrap) Check(ctx context.Context) {
	o, err := b.Ensure(ctx)
	marker, merr := b.readMarker(ctx)
	seen := false
	if err == nil && o.Client != nil {
		b.mu.Lock()
		seen = b.presented[Fingerprint(o.Client)]
		b.mu.Unlock()
	}
	if seen && o.Pending != "" {
		if cerr := b.ClearPending(ctx, Fingerprint(o.Client)); cerr != nil {
			b.logger().Error(cerr, "Scaler TLS: cannot clear the pending KEDA copy")
		} else {
			o.Pending = ""
		}
	}
	b.mu.Lock()
	b.markerNow, b.markerErr = marker, merr
	b.record(o, err)
	if err == nil && b.tlsOn && o.Serving != nil && o.CA != nil && bytes.Equal(o.CA.Raw, b.clientCA.Raw) {
		if cur := b.serving.Load(); cur == nil || !bytes.Equal(cur.Leaf.Raw, o.Serving.cert.Raw) {
			b.serving.Store(o.Serving.tlsCertificate())
			b.servedTrust = o.Trust
			b.logger().Info("Scaler TLS: serving a new certificate", "notAfter", o.Serving.cert.NotAfter)
		}
	}
	b.mu.Unlock()
	b.publish()
}

// record keeps the outcome of a pass. A failed pass keeps the previous
// outcome. Call with mu held.
func (b *Bootstrap) record(o *Outcome, err error) {
	b.lastErr = err
	if err != nil {
		b.logger().Error(err, "Scaler TLS: cannot provision the certificates")
		return
	}
	b.last = o
	if o.CAMinted {
		b.logger().Info("Scaler TLS: minted a new CA", "secret", CASecretName, "caSha256", Fingerprint(o.CA))
	}
	if o.ServingIssued {
		b.logger().Info("Scaler TLS: issued the serving certificate", "secret", ServingSecretName,
			"notAfter", o.Serving.cert.NotAfter)
	}
	if o.ClientIssued {
		b.logger().Info("Scaler TLS: issued KEDA's client certificate", "secret", ClientSecretName)
	}
	for _, p := range o.Problems {
		b.logger().Info("Scaler TLS: " + p)
	}
}

// GetCertificate serves the current serving certificate; pass it as
// tls.Config.GetCertificate.
func (b *Bootstrap) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := b.serving.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("scaler TLS: no serving certificate")
}

// ObserveHandshake records a handshake the scaler accepted; call it from
// tls.Config.VerifyConnection only after the client is verified. KEDA
// presenting the client certificate in its Secret ends the notice that KEDA's
// copy must be replaced.
func (b *Bootstrap) ObserveHandshake(cs tls.ConnectionState) {
	if len(cs.PeerCertificates) == 0 {
		return
	}
	fp := Fingerprint(cs.PeerCertificates[0])
	b.mu.Lock()
	if b.presented[fp] {
		b.mu.Unlock()
		return
	}
	if b.presented == nil || len(b.presented) > 16 {
		b.presented = map[string]bool{}
	}
	b.presented[fp] = true
	wanted := b.last != nil && b.last.Pending != "" && Fingerprint(b.last.Client) == fp
	b.mu.Unlock()
	if wanted {
		b.logger().Info("Scaler TLS: KEDA presents the client certificate in its Secret")
		b.publish()
	}
}

// readMarker reads ConfigMap MarkerConfigMapName. Any error but NotFound is
// markerUnknown, with the error.
func (b *Bootstrap) readMarker(ctx context.Context) (markerState, error) {
	cm := &corev1.ConfigMap{}
	err := b.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: MarkerConfigMapName}, cm)
	if apierrors.IsNotFound(err) {
		return markerAbsent, nil
	}
	if err != nil {
		return markerUnknown, fmt.Errorf("reading ConfigMap %s in %s: %w", MarkerConfigMapName, b.Namespace, err)
	}
	if _, ok := cm.Labels[OwnerLabel]; !ok {
		return markerForeign, fmt.Errorf("ConfigMap %s in %s has no %s label, so it is not KEDA's side as the release file or the installer writes it",
			MarkerConfigMapName, b.Namespace, OwnerLabel)
	}
	return markerPresent, nil
}

// publish sends the state and the action to Status.
func (b *Bootstrap) publish() {
	if b.Status == nil {
		return
	}
	b.mu.Lock()
	state, action := b.describe()
	trust := b.servedTrust
	if !b.tlsOn && b.last != nil && b.last.Usable() {
		trust = b.last.Trust
	}
	b.mu.Unlock()
	b.Status.SetScalerTLS(state, action)
	caPEM, sha := publicCerts(trust)
	b.Status.SetScalerCA(caPEM, sha)
}

// publicCerts re-encodes the CERTIFICATE blocks of b, and nothing else, so no
// key a Secret's field might also hold is ever published; with the sha256 of
// the first. Both "" when b holds no certificate.
func publicCerts(b []byte) (string, string) {
	var out []byte
	var first *x509.Certificate
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if first == nil {
			first = c
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	if first == nil {
		return "", ""
	}
	return string(out), Fingerprint(first)
}

// describe returns the state and the action, the action as numbered steps,
// one per line. Call with mu held.
func (b *Bootstrap) describe() (state, action string) {
	switch {
	case b.tlsOn:
		state = fmt.Sprintf("mutual TLS: Secrets %s, %s and %s; client identity %s; CA sha256 %s",
			CASecretName, ServingSecretName, ClientSecretName, b.ClientSAN, Fingerprint(b.clientCA))
		if c := b.serving.Load(); c != nil && c.Leaf != nil {
			state += "; serving certificate expires " + c.Leaf.NotAfter.UTC().Format(time.RFC3339)
		}
	case b.last == nil:
		state = "plaintext: the scaler TLS Secrets could not be provisioned"
	case !b.last.Usable():
		state = "plaintext: the scaler TLS certificates are not usable"
	case b.markerAtStart == markerForeign:
		state = "plaintext: " + b.markerErr.Error()
	default:
		state = "plaintext: KEDA's side of mutual TLS was not in place at start (no ConfigMap " + MarkerConfigMapName + ")"
	}

	var steps []string
	if b.lastErr != nil {
		steps = append(steps, fmt.Sprintf("the latest check failed (%v): the controller needs get and update on "+
			"Secrets %s, %s and %s in %s (Role scaling-manager-scaler-tls-role in the release files)",
			b.lastErr, CASecretName, ServingSecretName, ClientSecretName, b.Namespace))
	}
	if b.last != nil {
		steps = append(steps, b.last.Problems...)
	}
	pending := ""
	if b.last != nil && b.last.Client != nil && !b.presented[Fingerprint(b.last.Client)] {
		pending = b.last.Pending
	}
	switch {
	case b.last == nil || !b.last.Usable():
	case !b.tlsOn && b.markerNow == markerPresent:
		steps = append(steps, "KEDA's side is in place now; restart the controller to serve mutual TLS: "+b.restartController())
	case !b.tlsOn:
		step := "wire KEDA to the scaler's certificates (a cluster admin), then restart the controller: " +
			b.wireCommand() + " && " + b.restartController()
		if pending == PendingCA {
			step += " ; the CA is new: if KEDA dialled this scaler before (a reinstall), restart KEDA's operator too: " + restartKEDA()
		}
		steps = append(steps, step)
	case !bytes.Equal(b.last.CA.Raw, b.clientCA.Raw):
		steps = append(steps, fmt.Sprintf("the scaler CA in Secret %s changed since the controller started (sha256 %s; "+
			"it verifies clients with %s): wire KEDA to it, then restart the controller and KEDA's operator: %s && %s && %s",
			CASecretName, Fingerprint(b.last.CA), Fingerprint(b.clientCA), b.wireCommand(), b.restartController(), restartKEDA()))
	case pending == PendingCA:
		steps = append(steps, fmt.Sprintf("KEDA's client certificate (Secret %s) was issued for a new CA; copy it to KEDA again "+
			"and restart KEDA's operator, which keeps the CA of its first connection (a cluster admin): %s && %s",
			ClientSecretName, b.wireCommand(), restartKEDA()))
	case pending == PendingClient:
		steps = append(steps, fmt.Sprintf("KEDA's client certificate (Secret %s) was issued again; copy it to KEDA "+
			"(a cluster admin); this notice clears when KEDA presents it, after its operator restarts: %s",
			ClientSecretName, b.wireCommand()))
	}
	for i := range steps {
		steps[i] = fmt.Sprintf("%d. %s", i+1, steps[i])
	}
	return state, strings.Join(steps, "\n")
}

// kedaNS is the shell expression for KEDA's namespace: the operator sets
// KEDA_NS, which the controller cannot read, and the command stops if not.
const kedaNS = `${KEDA_NS:?set KEDA_NS to the namespace KEDA runs in: keda by default, openshift-keda on OpenShift}`

// wireCommand copies KEDA's client certificate into KEDA's namespace and
// applies the release file that points KEDA at it and writes the marker:
// the documented cluster-admin step. Only the three data keys are copied, and
// with a server-side apply, so no last-applied annotation holds a second copy
// of the private key. The ca.crt KEDA trusts is the client Secret's, or the
// OpenShift service CA bundle when the service CA issued the serving
// certificate. Call with mu held.
func (b *Bootstrap) wireCommand() string {
	ns := b.Namespace
	copyName := "scaling-manager-external-scaler-" + ns
	caField := `{{index .data "ca.crt"}}`
	prefix, filter := "", ""
	if b.last != nil && b.last.TrustFrom == TrustServiceCA {
		caField = "__SERVICE_CA__"
		prefix = fmt.Sprintf(`SERVICE_CA=$(kubectl get configmap %s -n %s -o jsonpath='{.data.service-ca\.crt}' | base64 | tr -d '\n') && `,
			ServiceCAConfigMap, ns)
		filter = ` | sed "s|__SERVICE_CA__|$SERVICE_CA|"`
	}
	tmpl := `{"apiVersion":"v1","kind":"Secret","type":"Opaque","metadata":{"name":"` + copyName + `","labels":{"` +
		"app.kubernetes.io/managed-by" + `":"llm-d-scaling-manager","` + "app.kubernetes.io/part-of" + `":"scaling-manager","` +
		OwnerLabel + `":"keda-client"}},` +
		`"data":{"tls.crt":"{{index .data "tls.crt"}}","tls.key":"{{index .data "tls.key"}}","ca.crt":"` + caField + `"}}`
	return fmt.Sprintf(`KEDA_NS="%s" && %skubectl get secret %s -n %s -o go-template='%s'%s | `+
		`kubectl apply --server-side --force-conflicts --field-manager=llm-d-scaling-manager -n "$KEDA_NS" -f - && `+
		`sed -e "s/__NAMESPACE__/%s/g" -e "s/__KEDA_NAMESPACE__/$KEDA_NS/g" scaling-manager-keda-auth.yaml | kubectl apply -f -`,
		kedaNS, prefix, ClientSecretName, ns, tmpl, filter, ns)
}

// restartController restarts the controller, which reads KEDA's side at start.
func (b *Bootstrap) restartController() string {
	return fmt.Sprintf("kubectl rollout restart deployment/%s -n %s", ControllerDeployment, b.Namespace)
}

// restartKEDA restarts KEDA's operator, which keeps the CA and client
// certificate of its first connection to a scaler address until it restarts.
func restartKEDA() string {
	return `kubectl rollout restart deployment/keda-operator -n "` + kedaNS + `"`
}
