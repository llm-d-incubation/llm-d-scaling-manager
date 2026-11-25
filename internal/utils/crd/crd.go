package crd

import (
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// serverGroupsAndResourcesIface is the minimal discovery surface needed by checkCRDInstalled.
type serverGroupsAndResourcesIface interface {
	ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error)
}

// CheckCRDInstalled reports whether a CRD with the given groupVersion and kind is
// registered in the cluster. It is called once at startup; dynamic CRD installation
// after the controller starts is not yet handled.
func CheckCRDInstalled(restConfig *rest.Config, groupVersion, kind string, logger logr.Logger) bool {
	installed, err := DetectCRDInstalled(restConfig, groupVersion, kind, logger)
	if err != nil {
		logger.Error(err, "failed to discover API resources",
			"groupVersion", groupVersion, "kind", kind)
	}
	return installed
}

// DetectCRDInstalled reports whether a CRD with the given groupVersion and kind
// is registered in the cluster, returning an error when discovery could not
// determine API resources at all.
func DetectCRDInstalled(restConfig *rest.Config, groupVersion, kind string, logger logr.Logger) (bool, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return false, err
	}
	return detectCRDInstalled(discoveryClient, groupVersion, kind, logger)
}

func checkCRDInstalled(disc serverGroupsAndResourcesIface, groupVersion, kind string, logger logr.Logger) bool {
	installed, err := detectCRDInstalled(disc, groupVersion, kind, logger)
	if err != nil {
		logger.Error(err, "failed to discover API resources",
			"groupVersion", groupVersion, "kind", kind)
	}
	return installed
}

func detectCRDInstalled(disc serverGroupsAndResourcesIface, groupVersion, kind string, logger logr.Logger) (bool, error) {
	_, apiLists, err := disc.ServerGroupsAndResources()
	if err != nil {
		// Partial errors are common (e.g. unavailable API services); continue if we got results.
		if apiLists == nil {
			return false, err
		}
		logger.V(1).Info("partial error discovering API resources (this is usually fine)", "error", err)
	}

	for _, apiList := range apiLists {
		if apiList.GroupVersion == groupVersion {
			for _, resource := range apiList.APIResources {
				if resource.Kind == kind {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// kedaGroupVersion and kedaKind name the KEDA ScaledObject kind.
const (
	kedaGroupVersion = "keda.sh/v1alpha1"
	kedaKind         = "ScaledObject"
)

// groupVersionResourcesIface is the discovery surface KEDAAbsent needs.
type groupVersionResourcesIface interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// KEDAAbsent reports whether KEDA's ScaledObject kind is known to be absent:
// true only when the API server answered that keda.sh/v1alpha1 is not served,
// or serves no ScaledObject. Any other error -- Forbidden, a timeout -- returns
// false with the error: what cannot be read is not absent, and a controller
// that read it as absent would force every model passive.
func KEDAAbsent(restConfig *rest.Config) (bool, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return false, err
	}
	return kedaAbsent(discoveryClient)
}

func kedaAbsent(disc groupVersionResourcesIface) (bool, error) {
	list, err := disc.ServerResourcesForGroupVersion(kedaGroupVersion)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, r := range list.APIResources {
		if r.Kind == kedaKind {
			return false, nil
		}
	}
	return true, nil
}

// CheckLeaderWorkerSetCRD reports whether the LeaderWorkerSet CRD is installed.
// TODO: checked once at startup; handle LWS installed after controller starts.
func CheckLeaderWorkerSetCRD(restConfig *rest.Config, logger logr.Logger) bool {
	return CheckCRDInstalled(restConfig, "leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet", logger)
}
