package main

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// The kubelet sends SIGKILL terminationGracePeriodSeconds after SIGTERM. If the
// manager is still stopping runnables then, the lease is never released
// (LeaderElectionReleaseOnCancel), and the successor waits the full
// LeaseDuration with GetMetrics unanswered. The manager's own budget has to fit
// inside the pod's, with room left for the process to exit.
func TestGracefulShutdownFitsGracePeriod(t *testing.T) {
	raw, err := os.ReadFile("../config/base/manager/deployment.yaml")
	require.NoError(t, err)
	var d appsv1.Deployment
	require.NoError(t, yaml.Unmarshal(raw, &d))
	require.NotNil(t, d.Spec.Template.Spec.TerminationGracePeriodSeconds,
		"the base Deployment must set terminationGracePeriodSeconds explicitly")

	grace := time.Duration(*d.Spec.Template.Spec.TerminationGracePeriodSeconds) * time.Second
	require.Less(t, managerGracefulShutdownTimeout+5*time.Second, grace,
		"manager graceful shutdown (%s) plus 5s must fit inside terminationGracePeriodSeconds (%s)",
		managerGracefulShutdownTimeout, grace)
}
