package controllerstatus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SettingsWait holds a controller that cannot start yet because its settings
// do not name Prometheus (--wait-for-settings, the one-manifest install,
// where the manifest is applied before the settings). Instead of exiting into
// the kubelet's crash back-off, it serves its health probes (live, not
// ready), says why in the status ConfigMap (ready, preflight.settings), and
// reads the settings again every Interval until Done says they are complete.
type SettingsWait struct {
	// Client reads the settings and writes the status ConfigMap.
	Client client.Client
	// Namespace is the controller's.
	Namespace string
	// SettingsName is the settings ConfigMap.
	SettingsName string
	// ProbeAddr is the health probe address the manager serves later
	// (--health-probe-bind-address); "" or "0" serves none.
	ProbeAddr string
	// Interval between reads; 5 s when zero.
	Interval time.Duration
	// Done reports whether the settings data is complete enough to start,
	// and why not.
	Done func(data map[string]string) (ok bool, why string)
	Log  logr.Logger
}

// Wait blocks until Done accepts the settings, and returns their data. It
// returns ctx's error when ctx ends first.
func (w *SettingsWait) Wait(ctx context.Context) (map[string]string, error) {
	interval := w.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	stop := w.serveProbes()
	defer stop()
	lastWhy := ""
	for {
		data, why, err := w.read(ctx)
		switch {
		case err != nil:
			why = err.Error()
		case why == "":
			w.Log.Info("The settings are complete; starting")
			return data, nil
		}
		if why != lastWhy {
			lastWhy = why
			w.Log.Info("Waiting for the settings before starting", "reason", why)
			if err := w.publish(ctx, why); err != nil {
				w.Log.Error(err, "Cannot write the waiting state to the status ConfigMap", "configMap", ConfigMapName)
				lastWhy = ""
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// read returns the settings data and why it is not complete ("" when it is).
func (w *SettingsWait) read(ctx context.Context) (map[string]string, string, error) {
	cm := &corev1.ConfigMap{}
	err := w.Client.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.SettingsName}, cm)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Sprintf("there is no ConfigMap %s in %s yet", w.SettingsName, w.Namespace), nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("reading ConfigMap %s in %s: %w", w.SettingsName, w.Namespace, err)
	}
	ok, why := w.Done(cm.Data)
	if ok {
		return cm.Data, "", nil
	}
	return cm.Data, why, nil
}

// WaitingReady is the ready value while waiting for the settings.
func WaitingReady(why string) string {
	return "not ready: settings (" + why + ")"
}

// publish writes ready and preflight.settings into the status ConfigMap,
// creating it when it does not exist. Only those two keys are written.
func (w *SettingsWait) publish(ctx context.Context, why string) error {
	data := map[string]string{
		KeyReady: WaitingReady(why),
		KeyPreflightPrefix + "settings": "fail: " + why + "; fix: set it in ConfigMap " + w.SettingsName + " in " + w.Namespace +
			"; the controller starts within seconds of it",
	}
	cm := &corev1.ConfigMap{}
	err := w.Client.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: ConfigMapName}, cm)
	if apierrors.IsNotFound(err) {
		return w.Client.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: w.Namespace,
				Labels: map[string]string{labelName: labelNameValue, labelComponent: componentValue, labelPartOf: labelPartOfValue}},
			Data: data,
		})
	}
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	for k, v := range data {
		cm.Data[k] = v
	}
	return w.Client.Update(ctx, cm)
}

// serveProbes serves /healthz (ok) and /readyz (not ready) on ProbeAddr while
// waiting, so the kubelet neither kills the container nor sends it traffic.
// The returned stop frees the port for the manager.
func (w *SettingsWait) serveProbes() func() {
	if w.ProbeAddr == "" || w.ProbeAddr == "0" {
		return func() {}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusServiceUnavailable)
		_, _ = rw.Write([]byte("waiting for the settings"))
	})
	lis, err := net.Listen("tcp", w.ProbeAddr)
	if err != nil {
		w.Log.Error(err, "Cannot serve the health probes while waiting for the settings", "addr", w.ProbeAddr)
		return func() {}
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			w.Log.Error(err, "The waiting health probe server stopped")
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}
