package main

import (
	"context"
	"crypto/tls"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scalertls"
)

// scalerTLSPrepareTimeout bounds the first pass of the scaler TLS bootstrap at
// start. Past it, a marker or Secrets still unknown stop the controller
// (Bootstrap.Prepare fails closed); the kubelet restarts it.
const scalerTLSPrepareTimeout = 30 * time.Second

// setupScalerTLSBootstrap provisions the external scaler's certificates
// (--external-scaler-tls-bootstrap), sets srv's TLS when the scaler is to serve
// mutual TLS, and adds the periodic pass to mgr. It reports through status.
func setupScalerTLSBootstrap(ctx context.Context, mgr manager.Manager, srv *scaler.Server,
	status scalertls.StatusSink, clientSAN string) error {
	boot := &scalertls.Bootstrap{
		Ensurer: scalertls.Ensurer{
			Reader:    mgr.GetAPIReader(),
			Writer:    mgr.GetClient(),
			Namespace: config.SystemNamespace(),
			ClientSAN: clientSAN,
		},
		Status: status,
		Log:    ctrl.Log.WithName("scaler-tls"),
	}
	prepareCtx, cancel := context.WithTimeout(ctx, scalerTLSPrepareTimeout)
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
		cfg.VerifyConnection = observed(cfg.VerifyConnection, boot.ObserveHandshake)
		srv.TLSConfig = cfg
		setupLog.Info("KEDA external scaler serves mutual TLS with the certificates the controller provisions",
			"caSecret", scalertls.CASecretName, "caSha256", scalertls.Fingerprint(ca), "allowedClient", clientSAN)
	} else {
		setupLog.Info("WARNING: the KEDA external scaler serves plaintext; scalerTLS and scalerTLSAction in "+
			"ConfigMap scaling-manager-controller-status say why and what to run",
			"marker", scalertls.MarkerConfigMapName)
	}
	return mgr.Add(boot)
}

// observed runs verify, then, for a connection it accepted, observe.
func observed(verify func(tls.ConnectionState) error, observe func(tls.ConnectionState)) func(tls.ConnectionState) error {
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
