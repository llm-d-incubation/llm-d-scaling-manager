// Package scalertls provisions the KEDA external scaler's mutual-TLS
// certificates from inside the controller, for an install made from the
// release files, which runs no installer script to mint them
// (docs/proposals/install-from-release.md, phase 3).
//
// It fills three kubernetes.io/tls Secrets in the controller's namespace, under
// the names and label deploy/lib/scaler_tls.sh uses, so an install made with
// make and one made from the release files hold the same objects and either
// adopts the other's:
//
//	scaling-manager-external-scaler-ca          the CA (tls.crt, tls.key, ca.crt)
//	scaling-manager-external-scaler-tls         the serving certificate
//	scaling-manager-external-scaler-client-tls  KEDA's client certificate
//
// It never creates a Secret: the release file PlaceholderFile creates the
// three, empty, and the controller may only get and update them by name. A
// grant to create Secrets would let the controller create a Secret of type
// kubernetes.io/service-account-token under one of those names and read the
// token the cluster fills in; the type of a Secret that exists cannot change.
//
// The rules, in the order they matter:
//
//   - A CA Secret that holds anything is never replaced. KEDA 2.18 keeps the
//     CA of its first connection to a scaler address in memory until its
//     operator restarts, so a new CA breaks every ScaledObject on that
//     address. An expiring or unusable CA is reported, with the command that
//     rotates it; the operator runs it.
//   - A CA is minted only into an empty placeholder.
//   - A leaf this package or the installer wrote (label
//     scaling-manager.llmd.ai/scaler-tls) is issued again from the CA when it
//     is empty, does not chain to the CA, names other hosts, or expires within
//     RenewBefore. A valid one is kept byte for byte.
//   - A Secret without that label, or one cert-manager or the OpenShift
//     service CA manages, is never written. When cert-manager manages any of
//     the three, or the CA is not the scaling manager's, nothing is written.
//   - Whatever is served is verified first against the CA KEDA is told to
//     trust: the CA's, or for a serving certificate the OpenShift service CA
//     issued, the service CA bundle.
package scalertls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// PlaceholderFile is the release file that creates the three Secrets empty.
	PlaceholderFile = "scaling-manager-scaler-tls.yaml"
	// PendingAnnotation, on the client Secret, says KEDA's copy of it must be
	// replaced: "ca" when the client certificate was issued for a new CA (KEDA's
	// operator must restart too), "client" when only the certificate is new. It
	// is written with the certificate and removed once KEDA presents it, by
	// whichever replica sees that.
	PendingAnnotation = "scaling-manager.llmd.ai/keda-copy-pending"
	// CAFilledAnnotation, on the CA Secret under Ensurer.Names, says the
	// controller filled it once, by a copy from an earlier install or by
	// minting (only with Ensurer.MigrateFrom set). A CA emptied after that is
	// a rotation: a new CA is minted, never the earlier install's copied
	// back. The rotate command empties the data and keeps the annotation.
	CAFilledAnnotation = "scaling-manager.llmd.ai/ca-filled"
	// ServiceCAConfigMap holds the OpenShift service CA bundle in every
	// namespace, under ServiceCAKey: what KEDA must trust when the service CA
	// issued the serving certificate.
	ServiceCAConfigMap = "openshift-service-ca.crt"
	// ServiceCAKey is the bundle's key in ServiceCAConfigMap.
	ServiceCAKey = "service-ca.crt"

	// certManagerAnnotation is set by cert-manager on every Secret it issues.
	certManagerAnnotation = "cert-manager.io/certificate-name"
	// openShiftServingAnnotation is set by the OpenShift service CA on the
	// serving Secret it issues.
	openShiftServingAnnotation = "service.beta.openshift.io/originating-service-name"
)

// Pending values: why KEDA's copy of the client Secret must be replaced.
const (
	PendingCA     = "ca"
	PendingClient = "client"
)

// TrustSource names what KEDA must trust for the serving certificate.
type TrustSource string

const (
	// TrustCA: the scaler CA, as the client Secret's ca.crt carries it.
	TrustCA TrustSource = "ca"
	// TrustServiceCA: the OpenShift service CA bundle (ServiceCAConfigMap).
	TrustServiceCA TrustSource = "service-ca"
)

// owner is who writes a Secret.
type owner int

const (
	ownerAbsent owner = iota
	// ownerUs: this package or the installer's openssl path wrote it, or it is
	// the release file's placeholder.
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

// empty reports whether s holds no certificate and no key: the placeholder.
func empty(s *corev1.Secret) bool {
	return s != nil && len(s.Data[corev1.TLSCertKey]) == 0 && len(s.Data[corev1.TLSPrivateKeyKey]) == 0
}

// Ensurer reads and, where the rules allow, fills the three Secrets.
type Ensurer struct {
	// Reader reads the Secrets; it should bypass the manager's cache
	// (mgr.GetAPIReader()), which holds no Secrets.
	Reader client.Reader
	// Writer updates them. It never creates one.
	Writer client.Client
	// Namespace is the controller's namespace.
	Namespace string
	// ClientSAN is the identity KEDA's client certificate carries and the
	// scaler accepts.
	ClientSAN string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Names are the three Secrets; nil is LegacyNames, the kubernetes.io/tls
	// placeholders every install but the one manifest makes.
	Names *SecretNames
	// MigrateFrom, when set, names an earlier install's three Secrets: while
	// the CA under Names is empty and was never filled (CAFilledAnnotation),
	// a CA and leaves there that are the scaling manager's are copied across
	// rather than a new CA minted, so KEDA, which keeps the CA of its first
	// connection, stays connected.
	MigrateFrom *SecretNames
}

// SecretNames names the scaler's three certificate Secrets.
type SecretNames struct {
	CA, Serving, Client string
}

// LegacyNames are the kubernetes.io/tls Secrets of the release files, smt
// and make: a re-apply of a file holding them empties them.
var LegacyNames = SecretNames{CA: CASecretName, Serving: ServingSecretName, Client: ClientSecretName}

// OpaqueNames are the Opaque Secrets with no data the one manifest
// (scaling-manager.yaml) carries, which a server-side re-apply keeps. New
// names, because the type of an existing Secret cannot change.
var OpaqueNames = SecretNames{CA: "scaling-manager-scaler-ca", Serving: "scaling-manager-scaler-serving",
	Client: "scaling-manager-scaler-client"}

func (e *Ensurer) names() SecretNames {
	if e.Names != nil {
		return *e.Names
	}
	return LegacyNames
}

// migrate copies an earlier install's certificates (MigrateFrom) into the
// Secrets under Names while the CA there is empty: each pair whose new
// Secret is an empty placeholder of ours and whose old one is ours and holds
// a certificate. It returns the Secrets it filled. An absent old Secret is
// skipped, and a new CA is then minted as on a fresh install. One it may not
// read is an error, never absent: no new CA is minted over one KEDA trusts.
func (e *Ensurer) migrate(ctx context.Context) ([]string, error) {
	if e.MigrateFrom == nil {
		return nil, nil
	}
	to, from := e.names(), *e.MigrateFrom
	newCA, err := e.get(ctx, to.CA)
	if err != nil || ownerOf(newCA) != ownerUs || !empty(newCA) {
		return nil, err
	}
	if _, filled := newCA.Annotations[CAFilledAnnotation]; filled {
		// Emptied after the controller filled it: a rotation.
		return nil, nil
	}
	oldCA, err := e.get(ctx, from.CA)
	if err != nil || ownerOf(oldCA) != ownerUs || empty(oldCA) {
		return nil, err
	}
	var copied []string
	// The leaves first and the CA last: a pass that stops halfway leaves the
	// new CA empty, and the next pass copies again.
	for _, p := range [][2]string{{from.Serving, to.Serving}, {from.Client, to.Client}, {from.CA, to.CA}} {
		src, err := e.get(ctx, p[0])
		if err != nil {
			return copied, err
		}
		dst, err := e.get(ctx, p[1])
		if err != nil {
			return copied, err
		}
		if ownerOf(src) != ownerUs || empty(src) || ownerOf(dst) != ownerUs || !empty(dst) {
			continue
		}
		if dst.Data == nil {
			dst.Data = map[string][]byte{}
		}
		for _, k := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey, corev1.ServiceAccountRootCAKey} {
			dst.Data[k] = src.Data[k]
		}
		if v, ok := src.Annotations[PendingAnnotation]; ok {
			if dst.Annotations == nil {
				dst.Annotations = map[string]string{}
			}
			dst.Annotations[PendingAnnotation] = v
		}
		if p[1] == to.CA {
			if dst.Annotations == nil {
				dst.Annotations = map[string]string{}
			}
			dst.Annotations[CAFilledAnnotation] = "copied from " + p[0]
		}
		if err := e.Writer.Update(ctx, dst); err != nil {
			if apierrors.IsConflict(err) {
				return copied, errRace
			}
			return copied, fmt.Errorf("copying Secret %s into %s in %s: %w", p[0], p[1], e.Namespace, err)
		}
		copied = append(copied, p[0]+" -> "+p[1])
	}
	return copied, nil
}

// Outcome is what one Ensure pass found and did.
type Outcome struct {
	// Serving is the serving certificate to present, nil when none is usable.
	// It was verified against Trust: it chains to it for server auth, has not
	// expired, and names the scaler's TLS address.
	Serving *keyPair
	// CA is the CA client certificates must chain to, nil when none is usable.
	CA *x509.Certificate
	// Trust is the PEM KEDA must trust for Serving, and TrustFrom where KEDA's
	// copy takes it from.
	Trust     []byte
	TrustFrom TrustSource
	// Client is KEDA's client certificate in its Secret when it is usable
	// (kept or issued); nil otherwise.
	Client *x509.Certificate
	// CAMinted: the CA placeholder was empty and this pass filled it.
	CAMinted bool
	// ServingIssued and ClientIssued: this pass wrote that leaf.
	ServingIssued, ClientIssued bool
	// Pending is PendingAnnotation on the client Secret: KEDA's copy must be
	// replaced. Read from the Secret, so every replica reports the same.
	Pending string
	// Adopted lists the Secrets kept as they were.
	Adopted []string
	// Migrated lists the Secrets this pass copied from an earlier install,
	// "<from> -> <to>" (Ensurer.MigrateFrom).
	Migrated []string
	// Problems are what keeps the scaler from mutual TLS, or will: each says
	// what is wrong and what to run. Empty when nothing needs a person.
	Problems []string
}

// Usable reports whether the outcome can serve mutual TLS.
func (o *Outcome) Usable() bool { return o.Serving != nil && o.CA != nil }

// errRace is a write that lost to another replica, or to a delete; the pass
// is run again.
var errRace = errors.New("another writer changed the scaler TLS Secrets")

// Ensure reads the Secrets, fills what the rules allow, and returns what can
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

// tlsAddressHost is the host TLS ScaledObjects dial; a serving certificate
// must name it.
func (e *Ensurer) tlsAddressHost() string { return ServiceName + "." + e.Namespace + ".svc" }

func (e *Ensurer) ensureOnce(ctx context.Context) (*Outcome, error) {
	now := e.now()
	migrated, err := e.migrate(ctx)
	if err != nil {
		return nil, err
	}
	caSecret, err := e.get(ctx, e.names().CA)
	if err != nil {
		return nil, err
	}
	servingSecret, err := e.get(ctx, e.names().Serving)
	if err != nil {
		return nil, err
	}
	clientSecret, err := e.get(ctx, e.names().Client)
	if err != nil {
		return nil, err
	}
	o := &Outcome{Migrated: migrated}
	// cert-manager renews what it issues and would overwrite a write here,
	// and a CA minted beside its leaves would sign nothing they chain to:
	// when it manages any of the three, nothing is written.
	for _, s := range []*corev1.Secret{caSecret, servingSecret, clientSecret} {
		if ownerOf(s) == ownerCertManager {
			return e.external(ctx, o, caSecret, servingSecret, clientSecret, ownerCertManager, now)
		}
	}

	var ca *keyPair
	switch caOwner := ownerOf(caSecret); {
	case caOwner == ownerAbsent:
		o.Problems = append(o.Problems, e.placeholderProblem(e.names().CA))
		return o, nil
	case caOwner == ownerUs && empty(caSecret):
		if ca, err = mintCA(e.Namespace, now); err != nil {
			return nil, fmt.Errorf("minting the scaler CA: %w", err)
		}
		var filled map[string]string
		if e.MigrateFrom != nil {
			filled = map[string]string{CAFilledAnnotation: "minted"}
		}
		if err := e.write(ctx, caSecret, ca, ca.certPEM, filled); err != nil {
			return nil, err
		}
		o.CAMinted = true
	case caOwner == ownerUs:
		ca, err = parsePair(caSecret.Data[corev1.TLSCertKey], caSecret.Data[corev1.TLSPrivateKeyKey])
		if err == nil {
			err = checkCA(ca.cert, now)
		}
		if err != nil {
			o.Problems = append(o.Problems, fmt.Sprintf(
				"Secret %s holds no usable CA (%v). The controller never replaces an existing CA: KEDA keeps the CA it first "+
					"connected with until its operator restarts. To have a new one minted: %s",
				e.names().CA, err, e.rotateCommand()))
			return o, nil
		}
		o.Adopted = append(o.Adopted, e.names().CA)
		if !ca.cert.NotAfter.After(now.Add(RenewBefore)) {
			o.Problems = append(o.Problems, fmt.Sprintf(
				"the scaler CA (Secret %s) expires %s. The controller never rotates it by itself. To rotate it: %s",
				e.names().CA, ca.cert.NotAfter.UTC().Format(time.RFC3339), e.rotateCommand()))
		}
	default:
		// Someone else's CA: their certificates too. Serve them if they are
		// usable, write nothing.
		return e.external(ctx, o, caSecret, servingSecret, clientSecret, caOwner, now)
	}
	o.CA, o.Trust, o.TrustFrom = ca.cert, pemOf(ca.cert), TrustCA

	// The serving certificate.
	switch ownerOf(servingSecret) {
	case ownerAbsent:
		o.Problems = append(o.Problems, e.placeholderProblem(e.names().Serving))
	case ownerUs:
		leaf, why := e.keep(servingSecret, ca, e.servingDNSNames(), x509.ExtKeyUsageServerAuth, false, now)
		if why == "" {
			o.Serving = leaf
			o.Adopted = append(o.Adopted, e.names().Serving)
			break
		}
		if leaf, err = issueLeaf(ca, "llmsm-external-scaler."+e.Namespace+".svc", e.servingDNSNames(),
			x509.ExtKeyUsageServerAuth, ServingValidity, now); err != nil {
			return nil, fmt.Errorf("issuing the scaler serving certificate: %w", err)
		}
		if err := e.write(ctx, servingSecret, leaf, pemOf(ca.cert), nil); err != nil {
			return nil, err
		}
		o.Serving = leaf
		o.ServingIssued = true
	default:
		if err := e.serveExternal(ctx, o, servingSecret, ca.cert, pemOf(ca.cert), now); err != nil {
			o.Problems = append(o.Problems, fmt.Sprintf(
				"Secret %s is %s and not usable (%v); the controller does not overwrite it. Replace it with the placeholder "+
					"and the controller issues one: kubectl delete secret %s -n %s && %s",
				e.names().Serving, ownerOf(servingSecret), err, e.names().Serving, e.Namespace, e.placeholderCommand(e.names().Serving)))
		}
	}

	// KEDA's client certificate.
	switch ownerOf(clientSecret) {
	case ownerAbsent:
		o.Problems = append(o.Problems, e.placeholderProblem(e.names().Client))
	case ownerUs:
		leaf, why := e.keep(clientSecret, ca, []string{e.ClientSAN}, x509.ExtKeyUsageClientAuth, true, now)
		if why == "" {
			o.Client = leaf.cert
			o.Pending = clientSecret.Annotations[PendingAnnotation]
			o.Adopted = append(o.Adopted, e.names().Client)
			break
		}
		// The CA KEDA trusts changes only when the certificate it holds does
		// not chain to this CA: then its operator must restart as well.
		pending := PendingCA
		if old, err := firstCert(clientSecret.Data[corev1.TLSCertKey]); err == nil && chains(old, ca.cert, x509.ExtKeyUsageClientAuth, now) {
			pending = PendingClient
		}
		if leaf, err = issueLeaf(ca, e.ClientSAN, []string{e.ClientSAN},
			x509.ExtKeyUsageClientAuth, ClientValidity, now); err != nil {
			return nil, fmt.Errorf("issuing KEDA's client certificate: %w", err)
		}
		if err := e.write(ctx, clientSecret, leaf, pemOf(ca.cert), map[string]string{PendingAnnotation: pending}); err != nil {
			return nil, err
		}
		o.Client = leaf.cert
		o.ClientIssued = true
		o.Pending = pending
	default:
		o.Problems = append(o.Problems, fmt.Sprintf(
			"Secret %s is %s; the controller does not overwrite it, and KEDA's copy must come from it. "+
				"If it is not KEDA's client certificate for this scaler, replace it with the placeholder and the controller issues one: "+
				"kubectl delete secret %s -n %s && %s",
			e.names().Client, ownerOf(clientSecret), e.names().Client, e.Namespace, e.placeholderCommand(e.names().Client)))
	}
	return o, nil
}

// keep returns the leaf in s when it may be kept, with why == "", or the reason
// it must be issued again. requireCA also requires its ca.crt to be the CA
// (KEDA's copy carries the CA it trusts from there).
func (e *Ensurer) keep(s *corev1.Secret, ca *keyPair, dns []string, eku x509.ExtKeyUsage, requireCA bool, now time.Time) (*keyPair, string) {
	if empty(s) {
		return nil, "empty"
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

// serveExternal verifies a serving Secret the controller does not write and,
// when it is usable, sets it in o with what KEDA must trust for it: the
// OpenShift service CA bundle for one the service CA issued, else ca.
func (e *Ensurer) serveExternal(ctx context.Context, o *Outcome, s *corev1.Secret, ca *x509.Certificate, caPEM []byte, now time.Time) error {
	if s == nil {
		return errors.New("absent")
	}
	leaf, err := parsePair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return err
	}
	trust, from := caPEM, TrustCA
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if ownerOf(s) == ownerServiceCA {
		bundle, err := e.serviceCABundle(ctx)
		if err != nil {
			return err
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(bundle) {
			return fmt.Errorf("ConfigMap %s holds no PEM certificate", ServiceCAConfigMap)
		}
		trust, from = bundle, TrustServiceCA
	}
	if err := verifyServing(leaf.cert, roots, e.tlsAddressHost(), now); err != nil {
		return err
	}
	o.Serving, o.Trust, o.TrustFrom = leaf, trust, from
	o.Adopted = append(o.Adopted, s.Name)
	return nil
}

// serviceCABundle reads the OpenShift service CA bundle.
func (e *Ensurer) serviceCABundle(ctx context.Context) ([]byte, error) {
	cm := &corev1.ConfigMap{}
	if err := e.Reader.Get(ctx, client.ObjectKey{Namespace: e.Namespace, Name: ServiceCAConfigMap}, cm); err != nil {
		return nil, fmt.Errorf("reading the OpenShift service CA (ConfigMap %s): %w", ServiceCAConfigMap, err)
	}
	if cm.Data[ServiceCAKey] == "" {
		return nil, fmt.Errorf("ConfigMap %s has no %s", ServiceCAConfigMap, ServiceCAKey)
	}
	return []byte(cm.Data[ServiceCAKey]), nil
}

// external handles a CA this package does not own, or a set cert-manager
// manages (caOwner says which): nothing is written, and the certificates are
// served when they verify against the CA, and the client Secret carries that
// CA for KEDA.
func (e *Ensurer) external(ctx context.Context, o *Outcome, caSecret, servingSecret, clientSecret *corev1.Secret,
	caOwner owner, now time.Time) (*Outcome, error) {
	var caCert *x509.Certificate
	err := errors.New("absent")
	if caSecret != nil {
		if caCert, err = firstCert(caSecret.Data[corev1.TLSCertKey]); err == nil {
			err = checkCA(caCert, now)
		}
	}
	if err != nil {
		o.Problems = append(o.Problems, fmt.Sprintf(
			"the scaler TLS Secrets are %s, so the controller writes none of them, and Secret %s holds no usable CA certificate (%v)",
			caOwner, e.names().CA, err))
		return o, nil
	}
	if clientSecret == nil || !sameCert(clientSecret.Data[corev1.ServiceAccountRootCAKey], caCert) {
		o.Problems = append(o.Problems, fmt.Sprintf(
			"the scaler TLS Secrets are %s, so the controller writes none of them, and Secret %s does not carry the CA of Secret %s "+
				"as its ca.crt, which is what KEDA's copy trusts", caOwner, e.names().Client, e.names().CA))
		return o, nil
	}
	o.CA = caCert
	o.Adopted = append(o.Adopted, e.names().CA, e.names().Client)
	if err := e.serveExternal(ctx, o, servingSecret, caCert, clientSecret.Data[corev1.ServiceAccountRootCAKey], now); err != nil {
		o.Problems = append(o.Problems, fmt.Sprintf(
			"the scaler TLS Secrets are %s, so the controller writes none of them, and Secret %s is not usable (%v)",
			caOwner, e.names().Serving, err))
	}
	return o, nil
}

// verifyServing checks a serving certificate KEDA verifies against roots: it
// chains for server auth, is valid now, and names host.
func verifyServing(leaf *x509.Certificate, roots *x509.CertPool, host string, now time.Time) error {
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		DNSName:     host,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("does not verify for %s against the CA KEDA trusts: %w", host, err)
	}
	return nil
}

// chains reports whether cert chains to ca for eku at now.
func chains(cert, ca *x509.Certificate, eku x509.ExtKeyUsage, now time.Time) bool {
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err := cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{eku}})
	return err == nil
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

// write updates cur, a Secret of ours, in place: its data, and annotations
// added; its type and labels are kept. A conflict, or a Secret deleted since
// it was read, is errRace.
func (e *Ensurer) write(ctx context.Context, cur *corev1.Secret, pair *keyPair, caPEM []byte, annotations map[string]string) error {
	next := cur.DeepCopy()
	next.Data = secretData(pair, caPEM)
	if len(annotations) > 0 && next.Annotations == nil {
		next.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		next.Annotations[k] = v
	}
	err := e.Writer.Update(ctx, next)
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return errRace
	}
	if err != nil {
		return fmt.Errorf("updating Secret %s in %s: %w", cur.Name, e.Namespace, err)
	}
	return nil
}

// ClearPending removes PendingAnnotation from the client Secret when it still
// holds the certificate with fingerprint presented: KEDA presented it, so its
// copy is in place.
func (e *Ensurer) ClearPending(ctx context.Context, presented string) error {
	s, err := e.get(ctx, e.names().Client)
	if err != nil || s == nil {
		return err
	}
	if _, ok := s.Annotations[PendingAnnotation]; !ok {
		return nil
	}
	cur, err := firstCert(s.Data[corev1.TLSCertKey])
	if err != nil || Fingerprint(cur) != presented {
		return nil
	}
	next := s.DeepCopy()
	delete(next.Annotations, PendingAnnotation)
	if err := e.Writer.Update(ctx, next); err != nil && !apierrors.IsConflict(err) {
		return fmt.Errorf("updating Secret %s in %s: %w", e.names().Client, e.Namespace, err)
	}
	return nil
}

// placeholderProblem says Secret name is missing and how to put it back.
func (e *Ensurer) placeholderProblem(name string) string {
	return fmt.Sprintf("Secret %s does not exist, and the controller may not create Secrets (only fill the placeholders "+
		"%s creates). Create it: %s", name, PlaceholderFile, e.placeholderCommand(name))
}

// placeholderCommand creates the empty placeholder for Secret name, as
// PlaceholderFile does.
func (e *Ensurer) placeholderCommand(name string) string {
	if e.Names != nil && *e.Names == OpaqueNames {
		return "kubectl apply --server-side -f scaling-manager.yaml   # the manifest holds it, Opaque with no data"
	}
	role := map[string]string{e.names().CA: "ca", e.names().Serving: "serving", e.names().Client: "client"}[name]
	return fmt.Sprintf("kubectl create secret generic %s -n %s --type=kubernetes.io/tls --from-literal=tls.crt= "+
		"--from-literal=tls.key= --from-literal=ca.crt= && kubectl label secret %s -n %s %s=%s app.kubernetes.io/part-of=scaling-manager",
		name, e.Namespace, name, e.Namespace, OwnerLabel, role)
}

// rotateCommand is what an operator runs to have a new CA minted.
func (e *Ensurer) rotateCommand() string {
	return fmt.Sprintf("kubectl patch secret %s -n %s --type merge -p '{\"data\":{\"tls.crt\":\"\",\"tls.key\":\"\",\"ca.crt\":\"\"}}' ; "+
		"the controller mints a new CA and certificates into it within its check interval and then says how to wire KEDA "+
		"to them (scalerTLSAction in ConfigMap scaling-manager-controller-status)", e.names().CA, e.Namespace)
}

// Fingerprint is the SHA-256 of a certificate's DER, in hex.
func Fingerprint(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// sameCert reports whether the first certificate in pemBytes is cert.
func sameCert(pemBytes []byte, cert *x509.Certificate) bool {
	c, err := firstCert(pemBytes)
	return err == nil && bytes.Equal(c.Raw, cert.Raw)
}

// sortedUnique returns names sorted, without duplicates.
func sortedUnique(names []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(names)))
}
