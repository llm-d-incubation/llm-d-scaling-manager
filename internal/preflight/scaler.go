package preflight

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Keys of ControllerStatusConfigMap the preflight reads.
const (
	// statusScalerCACert is the scaler CA's public certificate (PEM), which
	// the controller publishes so the serving chain can be checked without
	// reading a Secret.
	statusScalerCACert = "scalerCACert"
	// statusScalerUnreachable is present while KEDA's handshakes are refused.
	statusScalerUnreachable = "scalerUnreachable"
)

// scalerPort is the external-scaler port of ScalerService.
const scalerPort = "9090"

// DialTLS opens a TLS connection to addr and returns the certificate chain
// the server presented, leaf first. It verifies nothing: the caller does. A
// server that asks for a client certificate and then refuses the empty one
// still presented its chain, so the chain is returned with the error.
func DialTLS(ctx context.Context, addr string) ([]*x509.Certificate, error) {
	var chain []*x509.Certificate
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// The chain is verified by the caller, against the scaler CA.
			InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection's caller, against the published scaler CA
			VerifyConnection: func(cs tls.ConnectionState) error {
				chain = cs.PeerCertificates
				return nil
			},
		},
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if conn != nil {
		_ = conn.Close()
	}
	if len(chain) > 0 {
		return chain, nil
	}
	if err == nil {
		err = errors.New("the server presented no certificate")
	}
	return nil, err
}

// checkScalerTLS: the scaler's serving chain, read by a TLS handshake to its
// Service, verifies against the scaler CA's public certificate that the
// controller publishes in its status ConfigMap. That is the CA KEDA is given,
// so a chain that does not verify is one KEDA rejects.
func checkScalerTLS(ctx context.Context, s *state) ([]Result, string) {
	ns := s.env.Namespace
	_, err := s.env.Kube.CoreV1().ConfigMaps(ns).Get(ctx, KEDAMarkerConfigMap, metav1.GetOptions{})
	if err != nil {
		absent, why := readVerdict(err, "ConfigMap "+KEDAMarkerConfigMap+" in "+ns)
		if absent {
			return nil, "the scaler serves plaintext (see keda.mtls)"
		}
		return []Result{{Status: Unknown, Detail: why}}, ""
	}
	status, err := s.env.Kube.CoreV1().ConfigMaps(ns).Get(ctx, ControllerStatusConfigMap, metav1.GetOptions{})
	if err != nil {
		absent, why := readVerdict(err, "ConfigMap "+ControllerStatusConfigMap+" in "+ns)
		if absent {
			why = "the controller has not run yet (no ConfigMap " + ControllerStatusConfigMap + "); run preflight again after the install"
		}
		return []Result{{Status: Unknown, Detail: why}}, ""
	}
	ca := status.Data[statusScalerCACert]
	if ca == "" {
		return []Result{{Status: Unknown,
			Detail: fmt.Sprintf("%s carries no %s: this controller does not publish its scaler CA, so the serving chain is not checked", ControllerStatusConfigMap, statusScalerCACert),
			Fix:    "upgrade the controller to a release that publishes " + statusScalerCACert}}, ""
	}
	host := ScalerService + "." + ns + ".svc"
	addr := net.JoinHostPort(host, scalerPort)
	if s.env.DialTLS == nil {
		return []Result{{Status: Unknown, Detail: "no TLS dialer configured"}}, ""
	}
	chain, err := s.env.DialTLS(ctx, addr)
	if err != nil {
		return []Result{{Status: Unknown,
			Detail: fmt.Sprintf("no TLS answer from %s (%v). The NetworkPolicy %s admits port 9090 from KEDA's namespace only, which drops this Job's connection", addr, err, ScalerNetworkPolicy)}}, ""
	}
	if err := verifyChain([]byte(ca), chain, host, s.env.Now()); err != nil {
		return []Result{{Status: Fail,
			Detail: fmt.Sprintf("the scaler at %s serves a chain that does not verify against its CA (%s in %s): %v. KEDA, which trusts that CA, rejects it", addr, statusScalerCACert, ControllerStatusConfigMap, err),
			Fix:    fmt.Sprintf("kubectl rollout restart deployment/%s -n %s   # the controller re-issues its serving certificate from its CA at start", ControllerDeployment, ns)}}, ""
	}
	return []Result{{Status: Pass, Detail: fmt.Sprintf("the scaler at %s serves a chain that verifies against its CA, valid until %s", addr, chain[0].NotAfter.UTC().Format(time.DateOnly))}}, ""
}
