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

// Setup provisions the external scaler's certificates
// (--external-scaler-tls-bootstrap), sets srv's TLS when the scaler is to serve
// mutual TLS, and adds the periodic pass to mgr. It reports through status. An
// error means the controller must not start.
//
// It lives here and not in cmd: make build and the Dockerfile compile
// cmd/main.go alone, so a helper in another file of package main is not built.
func Setup(ctx context.Context, mgr manager.Manager, srv *scaler.Server, status StatusSink, clientSAN string) error {
	boot := &Bootstrap{
		Ensurer: Ensurer{
			Reader:    mgr.GetAPIReader(),
			Writer:    mgr.GetClient(),
			Namespace: config.SystemNamespace(),
			ClientSAN: clientSAN,
		},
		Status: status,
		Log:    ctrl.Log.WithName("scaler-tls"),
	}
	prepareCtx, cancel := context.WithTimeout(ctx, PrepareTimeout)
	defer cancel()
	setupLog := ctrl.Log.WithName("setup")
	ca, err := boot.Prepare(prepareCtx)
	if err != nil {
		return err
	}
	if ca != nil {
		cfg, err := scaler.NewMutualTLSConfig(boot.GetCertificate, ca, []string{clientSAN})
		if err != nil {
			return err
		}
		cfg.VerifyConnection = Observed(cfg.VerifyConnection, boot.ObserveHandshake)
		srv.TLSConfig = cfg
		setupLog.Info("KEDA external scaler serves mutual TLS with the certificates the controller provisions",
			"caSecret", CASecretName, "caSha256", Fingerprint(ca), "allowedClient", clientSAN)
	} else {
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
