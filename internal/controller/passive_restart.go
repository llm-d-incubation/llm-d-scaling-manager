package controller

import (
	"context"
	"fmt"
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// warnIfPassiveChanged warns, once per change, when the manager ConfigMap's
// LLMSM_PASSIVE (config.ManagerConfigPassive, the loader's reading) differs from
// the value this process read at start: the flag is read only then, so the
// edit does nothing until the controller restarts. An environment variable
// overrides the ConfigMap, so then there is nothing to warn about.
//
// r.passiveWarned is not locked: the reconciler runs with the default
// MaxConcurrentReconciles of 1 (SetupWithManager sets no other), so Reconcile
// calls never overlap.
func (r *ConfigMapReconciler) warnIfPassiveChanged(ctx context.Context, cm *corev1.ConfigMap) {
	if r.Config == nil || os.Getenv("LLMSM_PASSIVE") != "" {
		return
	}
	want, _ := config.ManagerConfigPassive(cm.Data)
	running := r.Config.PassiveDefault()
	if want == running {
		r.passiveWarned = ""
		return
	}
	key := strconv.FormatBool(want)
	if r.passiveWarned == key {
		return
	}
	r.passiveWarned = key
	msg := fmt.Sprintf("LLMSM_PASSIVE in %s is now %t and this controller runs with %t: it is read only at start, so restart the controller to apply it",
		cm.Name, want, running)
	log.FromContext(ctx).Info("Restart required: "+msg, "namespace", cm.Namespace, "configMap", cm.Name)
	if r.Recorder != nil {
		r.Recorder.Event(cm, corev1.EventTypeWarning, constants.K8SEventPassiveRestartRequired, msg)
	}
}
