// Package scalertls provisions the KEDA external scaler's mutual-TLS
// certificates from inside the controller, for an install made from the
// release files, which runs no installer script to mint them
// (docs/proposals/install-from-release.md, phase 3).
//
// It keeps three kubernetes.io/tls Secrets in the controller's namespace, under
// the names and label deploy/lib/scaler_tls.sh uses, so an install made with
// make and one made from the release files hold the same objects and either
// adopts the other's:
//
//	scaling-manager-external-scaler-ca          the CA (tls.crt, tls.key, ca.crt)
//	scaling-manager-external-scaler-tls         the serving certificate
//	scaling-manager-external-scaler-client-tls  KEDA's client certificate
//
// The rules, in the order they matter:
//
//   - A CA Secret that exists is never replaced, whatever it holds. KEDA 2.18
//     keeps the CA of its first connection to a scaler address in memory until
//     its operator restarts, so a new CA breaks every ScaledObject on that
//     address. An expiring or unusable CA is reported, with the command that
//     rotates it; the operator runs it.
//   - A CA is minted only when its Secret does not exist.
//   - A leaf this package or the installer wrote (label
//     scaling-manager.llmd.ai/scaler-tls) is re-issued from the CA when it is
//     missing, does not chain to the CA, names other hosts, or expires within
//     RenewBefore. A valid one is kept byte for byte.
//   - A Secret without that label, or one cert-manager or the OpenShift
//     service CA manages, is never written. When the CA is one of those, no
//     Secret is written at all: the certificates are whoever owns the CA's.
package scalertls

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Names and labels shared with deploy/lib/scaler_tls.sh.
const (
	// CASecretName is the CA Secret (LLMSM_SCALER_CA_SECRET).
	CASecretName = "scaling-manager-external-scaler-ca"
	// ServingSecretName is the serving certificate Secret (LLMSM_SCALER_TLS_SECRET).
	ServingSecretName = "scaling-manager-external-scaler-tls"
	// ClientSecretName is KEDA's client certificate Secret (LLMSM_SCALER_CLIENT_SECRET).
	ClientSecretName = "scaling-manager-external-scaler-client-tls"
	// MarkerConfigMapName records that KEDA's side of mutual TLS is in place
	// (LLMSM_SCALER_KEDA_MARKER; the release file scaling-manager-keda-auth.yaml).
	MarkerConfigMapName = "scaling-manager-scaler-keda-auth"
	// OwnerLabel marks a Secret this package or the installer wrote; its value
	// is the Secret's role (ca, serving, client), or cert-manager.
	OwnerLabel = "scaling-manager.llmd.ai/scaler-tls"
	// ServiceName is the scaler Service the serving certificate names.
	ServiceName = "scaling-manager-external-scaler"

	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "llm-d-scaling-manager"
	partOfLabel    = "app.kubernetes.io/part-of"
	partOfValue    = "scaling-manager"

	// certManagerAnnotation is set by cert-manager on every Secret it issues.
	certManagerAnnotation = "cert-manager.io/certificate-name"
	// openShiftServingAnnotation is set by the OpenShift service CA on the
	// serving Secret it issues.
	openShiftServingAnnotation = "service.beta.openshift.io/originating-service-name"
)

// owner is who writes a Secret.
type owner int

const (
	ownerAbsent owner = iota
	// ownerUs: this package or the installer's openssl path wrote it.
	ownerUs
	// ownerCertManager: cert-manager issues it, directly or through the
	// installer's cert-manager path (which also stamps OwnerLabel).
	ownerCertManager
	// ownerServiceCA: the OpenShift service CA issues it.
	ownerServiceCA
	// ownerForeign: someone else's, with none of the marks above.
	ownerForeign
)

func (o owner) String() string {
	switch o {
	case ownerUs:
		return "the scaling manager's"
	case ownerCertManager:
		return "managed by cert-manager"
	case ownerServiceCA:
		return "issued by the OpenShift service CA"
	case ownerForeign:
		return "not the scaling manager's (no " + OwnerLabel + " label)"
	}
	return "absent"
}

// ownerOf says who writes s. cert-manager is checked before the label: the
// installer's cert-manager path stamps the label on what cert-manager issues,
// and cert-manager would overwrite anything written here.
func ownerOf(s *corev1.Secret) owner {
	if s == nil {
		return ownerAbsent
	}
	if _, ok := s.Annotations[certManagerAnnotation]; ok || s.Labels[OwnerLabel] == "cert-manager" {
		return ownerCertManager
	}
	for _, ref := range s.OwnerReferences {
		if strings.HasPrefix(ref.APIVersion, "cert-manager.io/") {
			return ownerCertManager
		}
	}
	if _, ok := s.Annotations[openShiftServingAnnotation]; ok {
		return ownerServiceCA
	}
	if _, ok := s.Labels[OwnerLabel]; ok {
		return ownerUs
	}
	return ownerForeign
}

// Ensurer reads and, where the rules allow, writes the three Secrets.
type Ensurer struct {
	// Reader reads the Secrets; it should bypass the manager's cache
	// (mgr.GetAPIReader()), which holds no Secrets.
	Reader client.Reader
	// Writer creates and updates them.
	Writer client.Client
	// Namespace is the controller's namespace.
	Namespace string
	// ClientSAN is the identity KEDA's client certificate carries and the
	// scaler accepts.
	ClientSAN string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Outcome is what one Ensure pass found and did.
type Outcome struct {
	// Serving is the serving certificate to present, nil when none is usable.
	Serving *keyPair
	// CA is the CA client certificates must chain to, nil when none is usable.
	CA *x509.Certificate
	// Client is KEDA's client certificate in its Secret, when it is ours and
	// valid (kept or issued); nil otherwise.
	Client *x509.Certificate
	// CAMinted: the CA Secret did not exist and this pass created it.
	CAMinted bool
	// ServingIssued and ClientIssued: this pass wrote that leaf.
	ServingIssued, ClientIssued bool
	// Adopted lists the Secrets kept as they were.
	Adopted []string
	// Problems are what keeps the scaler from mutual TLS, or will: each says
	// what is wrong and what to run. Empty when nothing needs a person.
	Problems []string
}

// Usable reports whether the outcome can serve mutual TLS.
func (o *Outcome) Usable() bool { return o.Serving != nil && o.CA != nil }

// errRace is a write that lost to another replica; the pass is run again.
var errRace = errors.New("another writer changed the scaler TLS Secrets")

// Ensure reads the Secrets, writes what the rules allow, and returns what can
// be served. An error means a Secret could not be read or written, and
// nothing about the scaler's TLS is known; the caller keeps what it serves.
func (e *Ensurer) Ensure(ctx context.Context) (*Outcome, error) {
	var err error
	for range 3 {
		var o *Outcome
		if o, err = e.ensureOnce(ctx); !errors.Is(err, errRace) {
			return o, err
		}
	}
	return nil, err
}

func (e *Ensurer) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// servingDNSNames are the names the serving certificate carries: the Service
// by the address TLS ScaledObjects use and by its fully qualified form
// (llmsm_scaler_dns_names).
func (e *Ensurer) servingDNSNames() []string {
	return []string{
		ServiceName + "." + e.Namespace + ".svc",
		ServiceName + "." + e.Namespace + ".svc.cluster.local",
	}
}

func (e *Ensurer) ensureOnce(ctx context.Context) (*Outcome, error) {
	now := e.now()
	caSecret, err := e.get(ctx, CASecretName)
	if err != nil {
		return nil, err
	}
	servingSecret, err := e.get(ctx, ServingSecretName)
	if err != nil {
		return nil, err
	}
	clientSecret, err := e.get(ctx, ClientSecretName)
	if err != nil {
		return nil, err
	}
	o := &Outcome{}
	// cert-manager renews what it issues and would overwrite a write here,
	// and a CA minted beside its leaves would sign nothing they chain to:
	// when it manages any of the three, nothing is written.
	for _, s := range []*corev1.Secret{caSecret, servingSecret, clientSecret} {
		if ownerOf(s) == ownerCertManager {
			return e.external(o, caSecret, servingSecret, clientSecret, ownerCertManager, now), nil
		}
	}

	var ca *keyPair
	switch caOwner := ownerOf(caSecret); caOwner {
	case ownerAbsent:
		if ca, err = mintCA(e.Namespace, now); err != nil {
			return nil, fmt.Errorf("minting the scaler CA: %w", err)
		}
		if err := e.create(ctx, CASecretName, "ca", ca, ca.certPEM); err != nil {
			return nil, err
		}
		o.CAMinted = true
	case ownerUs:
		ca, err = parsePair(caSecret.Data[corev1.TLSCertKey], caSecret.Data[corev1.TLSPrivateKeyKey])
		if err == nil {
			err = checkCA(ca, now)
		}
		if err != nil {
			o.Problems = append(o.Problems, fmt.Sprintf(
				"Secret %s holds no usable CA (%v). The controller never replaces an existing CA: KEDA keeps the CA it first "+
					"connected with until its operator restarts. To have a new one minted: %s",
				CASecretName, err, e.rotateCommand()))
			return o, nil
		}
		o.Adopted = append(o.Adopted, CASecretName)
		if !ca.cert.NotAfter.After(now.Add(RenewBefore)) {
			o.Problems = append(o.Problems, fmt.Sprintf(
				"the scaler CA (Secret %s) expires %s. The controller never rotates it by itself. To rotate it: %s",
				CASecretName, ca.cert.NotAfter.UTC().Format(time.RFC3339), e.rotateCommand()))
		}
	default:
		// Someone else's CA: their certificates too. Serve them if they are
		// usable, write nothing.
		return e.external(o, caSecret, servingSecret, clientSecret, caOwner, now), nil
	}
	o.CA = ca.cert

	// The serving certificate.
	switch ownerOf(servingSecret) {
	case ownerAbsent, ownerUs:
		leaf, why := e.keep(servingSecret, ca, e.servingDNSNames(), x509.ExtKeyUsageServerAuth, false, now)
		if why == "" {
			o.Serving = leaf
			o.Adopted = append(o.Adopted, ServingSecretName)
			break
		}
		if leaf, err = issueLeaf(ca, "llmsm-external-scaler."+e.Namespace+".svc", e.servingDNSNames(),
			x509.ExtKeyUsageServerAuth, ServingValidity, now); err != nil {
			return nil, fmt.Errorf("issuing the scaler serving certificate: %w", err)
		}
		if err := e.write(ctx, servingSecret, ServingSecretName, "serving", leaf, ca.certPEM); err != nil {
			return nil, err
		}
		o.Serving = leaf
		o.ServingIssued = true
	default:
		leaf, err := usableExternal(servingSecret, now)
		if err != nil {
			o.Problems = append(o.Problems, fmt.Sprintf(
				"Secret %s is %s and not usable (%v); the controller does not overwrite it. Delete it and the controller issues one: "+
					"kubectl delete secret %s -n %s", ServingSecretName, ownerOf(servingSecret), err, ServingSecretName, e.Namespace))
		} else {
			o.Serving = leaf
			o.Adopted = append(o.Adopted, ServingSecretName)
		}
	}

	// KEDA's client certificate.
	switch ownerOf(clientSecret) {
	case ownerAbsent, ownerUs:
		leaf, why := e.keep(clientSecret, ca, []string{e.ClientSAN}, x509.ExtKeyUsageClientAuth, true, now)
		if why == "" {
			o.Client = leaf.cert
			o.Adopted = append(o.Adopted, ClientSecretName)
			break
		}
		if leaf, err = issueLeaf(ca, e.ClientSAN, []string{e.ClientSAN},
			x509.ExtKeyUsageClientAuth, ClientValidity, now); err != nil {
			return nil, fmt.Errorf("issuing KEDA's client certificate: %w", err)
		}
		if err := e.write(ctx, clientSecret, ClientSecretName, "client", leaf, ca.certPEM); err != nil {
			return nil, err
		}
		o.Client = leaf.cert
		o.ClientIssued = true
	default:
		o.Problems = append(o.Problems, fmt.Sprintf(
			"Secret %s is %s; the controller does not overwrite it, and KEDA's copy must come from it. "+
				"If it is not KEDA's client certificate for this scaler, delete it and the controller issues one: kubectl delete secret %s -n %s",
			ClientSecretName, ownerOf(clientSecret), ClientSecretName, e.Namespace))
	}
	return o, nil
}

// keep returns the leaf in s when it may be kept, with why == "", or the reason
// it must be issued again. requireCA also requires its ca.crt to be the CA
// (KEDA's copy carries the CA it trusts from there).
func (e *Ensurer) keep(s *corev1.Secret, ca *keyPair, dns []string, eku x509.ExtKeyUsage, requireCA bool, now time.Time) (*keyPair, string) {
	if s == nil {
		return nil, "absent"
	}
	leaf, err := parsePair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, err.Error()
	}
	if err := checkLeaf(leaf, ca.cert, dns, eku, now); err != nil {
		return nil, err.Error()
	}
	if requireCA && !sameCert(s.Data[corev1.ServiceAccountRootCAKey], ca.cert) {
		return nil, "its ca.crt is not the CA"
	}
	return leaf, ""
}

// external handles a CA this package does not own, or a set cert-manager
// manages (caOwner says which): nothing is written, and the certificates are
// served when they are usable.
func (e *Ensurer) external(o *Outcome, caSecret, servingSecret, clientSecret *corev1.Secret, caOwner owner, now time.Time) *Outcome {
	var caCert *x509.Certificate
	err := errors.New("absent")
	if caSecret != nil {
		caCert, err = firstCert(caSecret.Data[corev1.TLSCertKey])
	}
	if err == nil && !now.Before(caCert.NotAfter) {
		err = fmt.Errorf("expired %s", caCert.NotAfter.UTC().Format(time.RFC3339))
	}
	if err != nil {
		o.Problems = append(o.Problems, fmt.Sprintf(
			"the scaler TLS Secrets are %s, so the controller writes none of them, and Secret %s holds no usable CA certificate (%v)",
			caOwner, CASecretName, err))
		return o
	}
	o.CA = caCert
	o.Adopted = append(o.Adopted, CASecretName)
	leaf, err := usableExternal(servingSecret, now)
	if err != nil {
		o.Problems = append(o.Problems, fmt.Sprintf(
			"the scaler TLS Secrets are %s, so the controller writes none of them, and Secret %s is not usable (%v)",
			caOwner, ServingSecretName, err))
		return o
	}
	o.Serving = leaf
	o.Adopted = append(o.Adopted, ServingSecretName)
	if clientSecret != nil {
		o.Adopted = append(o.Adopted, ClientSecretName)
	}
	return o
}

// usableExternal returns the serving pair in s when it parses, matches its key
// and has not expired.
func usableExternal(s *corev1.Secret, now time.Time) (*keyPair, error) {
	if s == nil {
		return nil, errors.New("absent")
	}
	leaf, err := parsePair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, err
	}
	if !now.Before(leaf.cert.NotAfter) {
		return nil, fmt.Errorf("expired %s", leaf.cert.NotAfter.UTC().Format(time.RFC3339))
	}
	return leaf, nil
}

// get reads Secret name, nil when it does not exist. Any other error, a
// Forbidden included, is returned: Forbidden is unknown, never absent.
func (e *Ensurer) get(ctx context.Context, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := e.Reader.Get(ctx, client.ObjectKey{Namespace: e.Namespace, Name: name}, s)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Secret %s in %s: %w", name, e.Namespace, err)
	}
	return s, nil
}

// secretData is the data of a kubernetes.io/tls Secret as the installer
// writes it.
func secretData(pair *keyPair, caPEM []byte) map[string][]byte {
	return map[string][]byte{
		corev1.TLSCertKey:              pair.certPEM,
		corev1.TLSPrivateKeyKey:        pair.keyPEM,
		corev1.ServiceAccountRootCAKey: caPEM,
	}
}

// create creates Secret name with role. A Secret created meanwhile by another
// replica is errRace: the pass runs again and adopts it.
func (e *Ensurer) create(ctx context.Context, name, role string, pair *keyPair, caPEM []byte) error {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: e.Namespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				partOfLabel:    partOfValue,
				OwnerLabel:     role,
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: secretData(pair, caPEM),
	}
	err := e.Writer.Create(ctx, s)
	if apierrors.IsAlreadyExists(err) {
		return errRace
	}
	if err != nil {
		return fmt.Errorf("creating Secret %s in %s: %w", name, e.Namespace, err)
	}
	return nil
}

// write creates Secret name, or updates cur (ours) in place: its data only,
// keeping its type, labels and annotations. A conflict is errRace.
func (e *Ensurer) write(ctx context.Context, cur *corev1.Secret, name, role string, pair *keyPair, caPEM []byte) error {
	if cur == nil {
		return e.create(ctx, name, role, pair, caPEM)
	}
	next := cur.DeepCopy()
	next.Data = secretData(pair, caPEM)
	err := e.Writer.Update(ctx, next)
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return errRace
	}
	if err != nil {
		return fmt.Errorf("updating Secret %s in %s: %w", name, e.Namespace, err)
	}
	return nil
}

// rotateCommand is what an operator runs to have a new CA minted.
func (e *Ensurer) rotateCommand() string {
	return fmt.Sprintf("kubectl delete secret %s -n %s ; the controller mints a new CA and certificates within its check "+
		"interval and then says how to wire KEDA to them (scalerTLSAction in ConfigMap scaling-manager-controller-status)",
		CASecretName, e.Namespace)
}

// Fingerprint is the SHA-256 of a certificate's DER, in hex.
func Fingerprint(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}
