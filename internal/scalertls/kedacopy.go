package scalertls

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KEDACopyRole is the Role in KEDA's namespace that lets the controller get
// and update KEDA's copy, by name, in the release manifest.
const KEDACopyRole = "scaling-manager-keda-copy"

// KEDA connection states (controllerstatus.KeyKEDAConnection).
const (
	KEDAConnectionNone    = "none"
	KEDAConnectionCurrent = "current"
	KEDAConnectionStale   = "stale"
)

// KEDASink receives how KEDA connects when the controller writes KEDA's copy
// itself. A StatusSink that also implements it is given it.
type KEDASink interface {
	SetKEDAConnection(connection string, copyStale bool)
}

// KEDACopy is KEDA's copy of the client certificate, which the controller
// writes itself on the install from the one release manifest
// (proposal PR #404, "KEDA's copy"): the manifest
// pre-creates the Secret in KEDA's namespace, Opaque and with no data, and a
// Role there that gives the controller get and update on it by name. The
// controller never creates a Secret, and reads nothing else in KEDA's
// namespace.
type KEDACopy struct {
	// Namespace is KEDA's namespace.
	Namespace string
	// SecretName is the pre-created Secret: the one the
	// ClusterTriggerAuthentication names.
	SecretName string
}

// KEDACopySecretName is the name of KEDA's copy for a controller in
// namespace: the name the ClusterTriggerAuthentication and the installer use.
func KEDACopySecretName(namespace string) string {
	return "scaling-manager-external-scaler-" + namespace
}

// copyKeys are the keys KEDA reads (the ClusterTriggerAuthentication).
var copyKeys = []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey, "ca.crt"}

// sync makes KEDA's copy hold the client Secret's certificate, key and CA.
// It returns markerPresent when the copy holds them, with stale false.
// markerAbsent means the copy cannot be made now (no client certificate yet,
// no pre-created Secret, or no right to it), and markerForeign that the
// Secret there is not the scaling manager's; err says why for both. Any other
// failure is markerUnknown. stale is true when the copy does not hold the
// current certificate.
func (k *KEDACopy) sync(ctx context.Context, r client.Reader, w client.Client, ns, clientName string) (state markerState, stale bool, err error) {
	var src corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: clientName}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			return markerAbsent, true, fmt.Errorf("the Secret %s in %s does not exist", clientName, ns)
		}
		return markerUnknown, true, fmt.Errorf("reading Secret %s in %s: %w", clientName, ns, err)
	}
	if len(src.Data[corev1.TLSCertKey]) == 0 {
		return markerAbsent, true, fmt.Errorf("the Secret %s in %s holds no client certificate yet", clientName, ns)
	}
	var dst corev1.Secret
	key := client.ObjectKey{Namespace: k.Namespace, Name: k.SecretName}
	if err := r.Get(ctx, key, &dst); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return markerAbsent, true, fmt.Errorf("the Secret %s in KEDA's namespace %s does not exist: the release manifest "+
				"creates it, empty, and the controller may not create Secrets; apply the manifest again", k.SecretName, k.Namespace)
		case apierrors.IsForbidden(err):
			return markerAbsent, true, fmt.Errorf("may not read Secret %s in KEDA's namespace %s: the controller needs Role %s "+
				"there, from the release manifest (is KEDA in another namespace? see the guide's KEDA namespace step): %w",
				k.SecretName, k.Namespace, KEDACopyRole, err)
		}
		return markerUnknown, true, fmt.Errorf("reading Secret %s in %s: %w", k.SecretName, k.Namespace, err)
	}
	if _, ok := dst.Labels[OwnerLabel]; !ok {
		return markerForeign, true, fmt.Errorf("the Secret %s in %s has no %s label, so it is not the copy the release manifest "+
			"creates; the controller does not write it", k.SecretName, k.Namespace, OwnerLabel)
	}
	same := true
	for _, kk := range copyKeys {
		if !bytes.Equal(dst.Data[kk], src.Data[kk]) {
			same = false
		}
	}
	if same {
		return markerPresent, false, nil
	}
	if dst.Data == nil {
		dst.Data = map[string][]byte{}
	}
	for _, kk := range copyKeys {
		dst.Data[kk] = src.Data[kk]
	}
	if err := w.Update(ctx, &dst); err != nil {
		if apierrors.IsForbidden(err) {
			return markerAbsent, true, fmt.Errorf("may not update Secret %s in KEDA's namespace %s: the controller needs Role %s there: %w",
				k.SecretName, k.Namespace, KEDACopyRole, err)
		}
		return markerUnknown, true, fmt.Errorf("updating Secret %s in %s: %w", k.SecretName, k.Namespace, err)
	}
	return markerPresent, false, nil
}
