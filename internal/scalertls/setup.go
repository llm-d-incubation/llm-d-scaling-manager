package scalertls

import (
	"context"
	"crypto/tls"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

// PrepareTimeout bounds the first pass of the bootstrap at start. Past it, a
// marker or Secrets still unknown stop the controller (Bootstrap.Prepare fails
// closed); the kubelet restarts it.
const PrepareTimeout = 30 * time.Second

// SetupOptions are the one-manifest choices of Setup; the zero value is
// every other install.
type SetupOptions struct {
	// KEDA: the controller writes KEDA's copy (--keda-namespace).
	KEDA *KEDACopy
	// Names: the Secrets, nil for LegacyNames (--external-scaler-tls-secrets).
	Names *SecretNames
	// Live: switch to mutual TLS, and adopt a new CA, with no restart
	// (--external-scaler-tls-live).
	Live bool
}

// Setup provisions the external scaler's certificates
// (--external-scaler-tls-bootstrap), sets srv's TLS when the scaler is to serve
// mutual TLS, and adds the periodic pass to mgr. It reports through status. An
// error means the controller must not start.
//
// It lives here and not in cmd: make build and the Dockerfile compile
// cmd/main.go alone, so a helper in another file of package main is not built.
func Setup(ctx context.Context, mgr manager.Manager, srv *scaler.Server, status StatusSink, clientSAN string, opts SetupOptions) error {
	keda, names := opts.KEDA, opts.Names
	boot := &Bootstrap{
		Ensurer: Ensurer{
			Reader:    mgr.GetAPIReader(),
			Writer:    mgr.GetClient(),
			Namespace: config.SystemNamespace(),
			ClientSAN: clientSAN,
			Names:     names,
		},
		Status: status,
		Log:    ctrl.Log.WithName("scaler-tls"),
		KEDA:   keda,
	}
	if opts.Live {
		switches := make(chan *tls.Config, 1)
		srv.TLSSwitch = switches
		boot.AllowedClients = []string{clientSAN}
		boot.SwitchTLS = func(cfg *tls.Config) {
			// One pending switch is enough: a newer config replaces it.
			select {
			case <-switches:
			default:
			}
			switches <- cfg
		}
	}
	if names != nil && *names != LegacyNames {
		// A move from an install with the kubernetes.io/tls Secrets: their CA
		// is copied across, so KEDA keeps the CA it connected with.
		boot.MigrateFrom = &LegacyNames
	}
	prepareCtx, cancel := context.WithTimeout(ctx, PrepareTimeout)
	defer cancel()
	setupLog := ctrl.Log.WithName("setup")
	ca, err := boot.Prepare(prepareCtx)
	if err != nil {
		return err
	}
	switch {
	case ca != nil && opts.Live:
		boot.AllowedClients = []string{clientSAN}
		srv.TLSConfig = boot.MutualTLSConfig()
		setupLog.Info("KEDA external scaler serves mutual TLS with the certificates the controller provisions; a new CA is adopted live",
			"caSecret", boot.names().CA, "caSha256", Fingerprint(ca), "allowedClient", clientSAN)
	case ca != nil:
		cfg, err := scaler.NewMutualTLSConfig(boot.GetCertificate, ca, []string{clientSAN})
		if err != nil {
			return err
		}
		cfg.VerifyConnection = Observed(cfg.VerifyConnection, boot.ObserveHandshake)
		srv.TLSConfig = cfg
		setupLog.Info("KEDA external scaler serves mutual TLS with the certificates the controller provisions",
			"caSecret", boot.names().CA, "caSha256", Fingerprint(ca), "allowedClient", clientSAN)
	default:
		setupLog.Info("WARNING: the KEDA external scaler serves plaintext; scalerTLS and scalerTLSAction in "+
			"ConfigMap scaling-manager-controller-status say why and what to run",
			"marker", MarkerConfigMapName)
	}
	return mgr.Add(boot)
}

// Observed runs verify, then, for a connection it accepted, observe.
func Observed(verify func(tls.ConnectionState) error, observe func(tls.ConnectionState)) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if verify != nil {
			if err := verify(cs); err != nil {
				return err
			}
		}
		observe(cs)
		return nil
	}
}
