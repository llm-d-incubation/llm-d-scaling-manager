package kueue

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ControllerVersion is what is known of the running Kueue controller's version.
type ControllerVersion struct {
	// Version is the release its image tag names, as "v<major>.<minor>.<patch>"
	// with any pre-release suffix; empty when unknown.
	Version string
	// Tested is true when Version is in TestedKueueVersions. False for an
	// unknown version too: check Known first.
	Tested bool
	// Known is false when the controller's Deployment could not be read
	// (Forbidden, absent, another namespace) or its image names no version, as
	// with a digest-only reference.
	Known bool
	// Down is true when no leader is known to run. With the leader-election
	// Lease readable, when its renewTime is older than twice its
	// leaseDurationSeconds; without a Lease (not found), when the Deployment
	// reports no ready replica. A Deployment and Lease that cannot be found or
	// read -- Forbidden included, as a namespace-scoped install without the
	// Kueue-namespace Role gets -- is Down as well: nothing shows that Kueue
	// admits or preempts. Kueue's API may still answer, but the figures stop
	// moving, so new borrowing stops (ReasonKueueControllerDown) and what is
	// borrowed is frozen.
	Down bool
}

// forbiddenTTL is how long a Forbidden read of the controller's Deployment or
// Lease is kept: RBAC changes rarely, and asking again every refresh is a call
// for nothing. Every other answer is read again next refresh, since the
// controller's liveness can change at any time.
const forbiddenTTL = 10 * time.Minute

// defaultLeaseDuration is client-go's leader-election lease duration, for a
// Lease that does not say its own.
const defaultLeaseDuration = 15 * time.Second

// semverTag matches the release a Kueue image tag names: v0.20.0, 0.20.1,
// v0.21.0-rc.1.
var semverTag = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)([-+][0-9A-Za-z.+-]*)?$`)

// ParseControllerVersion returns the release an image reference's tag names,
// normalized to "v<major>.<minor>.<patch>[suffix]", and false when the
// reference has no tag or the tag names no release. A digest after the tag
// (repo:tag@sha256:...) is ignored.
func ParseControllerVersion(image string) (string, bool) {
	ref, _, _ := strings.Cut(image, "@")
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash {
		return "", false // no tag: a registry port is not one
	}
	m := semverTag.FindStringSubmatch(ref[colon+1:])
	if m == nil {
		return "", false
	}
	return "v" + m[1] + "." + m[2] + "." + m[3] + m[4], true
}

// VersionTested reports whether version, as ParseControllerVersion returns
// it, is a release of a minor in TestedKueueVersions. A pre-release of a
// tested minor (v0.20.0-rc.1) is not that minor's release, and is not tested
// (K6); build metadata (+...) does not make one.
func VersionTested(version string) bool {
	m := semverTag.FindStringSubmatch(version)
	if m == nil || strings.HasPrefix(m[4], "-") {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return slices.Contains(TestedKueueVersions, fmt.Sprintf("v%d.%d", major, minor))
}

// readOutcome classifies one get of the controller's Deployment or Lease.
type readOutcome int

const (
	readOK readOutcome = iota
	// readAbsent: NotFound, or a kind the cluster does not serve.
	readAbsent
	readForbidden
	// readTransient: any other failure, which may pass by the next read.
	readTransient
)

func outcomeOf(err error) readOutcome {
	switch {
	case err == nil:
		return readOK
	case apierrors.IsForbidden(err):
		return readForbidden
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return readAbsent
	}
	return readTransient
}

// controllerVersion reads the Kueue controller's version from its Deployment's
// image, and whether a leader runs from its leader-election Lease, falling
// back to the Deployment's ready replicas when there is no Lease, or when the
// Lease was last renewed long before the Deployment last rolled out (K7: a
// controller run with leader election off leaves the old Lease behind). Both
// are read on every refresh. A Forbidden get is not repeated for forbiddenTTL,
// each object on its own (G2): a Forbidden Deployment never freezes the
// Lease, which shows whether a leader runs. A failure that may pass keeps the
// last answer. Callers must hold r.mu.
func (r *Reader) controllerVersion(ctx context.Context) ControllerVersion {
	now := r.opts.now()
	ns := r.opts.ControllerNamespace
	if ns == "" {
		ns = DefaultControllerNamespace
	}
	get := func(gvk schema.GroupVersionKind, name string, deniedAt *time.Time) (*unstructured.Unstructured, readOutcome) {
		if !deniedAt.IsZero() && now.Sub(*deniedAt) < forbiddenTTL {
			return nil, readForbidden
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		out := outcomeOf(r.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, u))
		*deniedAt = time.Time{}
		if out == readForbidden {
			*deniedAt = now
		}
		return u, out
	}
	d, dOut := get(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, ControllerDeployment, &r.deploymentDeniedAt)
	lease, lOut := get(schema.GroupVersionKind{Group: "coordination.k8s.io", Version: "v1", Kind: "Lease"}, ControllerLease, &r.leaseDeniedAt)
	if dOut == readTransient || lOut == readTransient {
		return r.version // may pass: asked again next read
	}

	var v ControllerVersion
	if dOut == readOK {
		v = versionOf(d)
	}
	switch {
	case lOut == readOK && dOut == readOK && leaseOrphaned(lease, d):
		// The Lease predates the running Deployment by far: the controller
		// runs without leader election, so the Deployment says whether it
		// runs.
		ready, _, _ := unstructured.NestedInt64(d.Object, "status", "readyReplicas")
		v.Down = ready < 1
	case lOut == readOK:
		v.Down = leaseExpired(lease, now)
	case lOut == readForbidden:
		// Forbidden is not absent: the Lease that would show a leader runs
		// cannot be read, so nothing does.
		v.Down = true
	case dOut == readOK:
		ready, _, _ := unstructured.NestedInt64(d.Object, "status", "readyReplicas")
		v.Down = ready < 1
	default:
		// Neither the Lease nor the Deployment found, or the Deployment
		// Forbidden: no sign of a running controller.
		v.Down = true
	}

	logger := kueueLogger(ctx)
	if v.Down != r.version.Down || r.versionAt.IsZero() {
		target := ns + "/" + ControllerDeployment
		switch {
		case v.Down:
			logger.Info("Cannot see a running Kueue controller (its leader Lease is not renewed, it has no ready replica, or neither can be found or read); new cohort borrowing is withheld (reason kueue-controller-down) and what is borrowed is frozen until it is back",
				"deployment", target, "lease", ns+"/"+ControllerLease,
				"deploymentRead", readName(dOut), "leaseRead", readName(lOut),
				"remedy", "for a namespace-scoped install, an admin re-runs make setup-prereqs to grant the Role in the Kueue namespace (config/components/kueue-reader)")
		case !r.versionAt.IsZero():
			logger.Info("The Kueue controller runs again; cohort borrowing is counted as before", "deployment", target)
		}
	}
	vers := func(c ControllerVersion) ControllerVersion { c.Down = false; return c }
	if vers(v) != vers(r.version) || r.versionAt.IsZero() {
		switch {
		case v.Down:
			// Logged above.
		case !v.Known:
			logger.Info("The Kueue controller's image names no version; cohort borrowing is withheld (reason kueue-version-unknown) unless kueue.assumeTestedVersion is set. Logged when the answer changes.",
				"deployment", ns+"/"+ControllerDeployment)
		case !v.Tested:
			logger.Info("The Kueue controller runs a version outside the tested range; cohort borrowing is withheld (reason kueue-version-untested) and the nominal quotas still apply",
				"version", v.Version, "tested", TestedKueueVersions)
		default:
			logger.V(1).Info("Kueue controller version", "version", v.Version)
		}
	}
	r.version, r.versionAt = v, now
	return v
}

// readName names an outcome for the log.
func readName(o readOutcome) string {
	switch o {
	case readOK:
		return "ok"
	case readAbsent:
		return "not-found"
	case readForbidden:
		return "forbidden"
	}
	return "failed"
}

// leaseRenewal is a leader-election Lease's renewTime and duration; ok is
// false when it has none that parses (never held).
func leaseRenewal(lease *unstructured.Unstructured) (renewed time.Time, duration time.Duration, ok bool) {
	raw, _, _ := unstructured.NestedString(lease.Object, "spec", "renewTime")
	if raw == "" {
		return time.Time{}, 0, false
	}
	renewed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, 0, false
	}
	duration = defaultLeaseDuration
	if secs, found, _ := unstructured.NestedInt64(lease.Object, "spec", "leaseDurationSeconds"); found && secs > 0 {
		duration = time.Duration(secs) * time.Second
	}
	return renewed, duration, true
}

// leaseExpired reports whether a leader-election Lease has not been renewed
// for twice its duration: no leader runs. A Lease without a renewTime has
// never been held.
func leaseExpired(lease *unstructured.Unstructured, now time.Time) bool {
	renewed, duration, ok := leaseRenewal(lease)
	return !ok || now.Sub(renewed) > 2*duration
}

// orphanedAfter is how long before the Deployment's last rollout a Lease must
// have been renewed last to be read as left behind (leaseOrphaned): far past
// any handover between an old leader and a new one.
const orphanedAfter = 10 * time.Minute

// leaseOrphaned reports whether the Lease was last renewed long before the
// Deployment last rolled out -- its Progressing condition's lastUpdateTime, or
// its creation when it records none (K7). Every leader of the running
// Deployment would have renewed it since, so none uses it: the controller
// runs with leader election off, and the stale Lease says nothing.
func leaseOrphaned(lease, d *unstructured.Unstructured) bool {
	renewed, duration, ok := leaseRenewal(lease)
	if !ok {
		return false
	}
	rollout := d.GetCreationTimestamp().Time
	conds, _, _ := unstructured.NestedSlice(d.Object, "status", "conditions")
	for _, raw := range conds {
		c, _ := raw.(map[string]interface{})
		if c["type"] != "Progressing" {
			continue
		}
		s, _ := c["lastUpdateTime"].(string)
		if at, err := time.Parse(time.RFC3339, s); err == nil && at.After(rollout) {
			rollout = at
		}
	}
	return !rollout.IsZero() && rollout.Sub(renewed) > max(orphanedAfter, 2*duration)
}

// versionOf reads the version from a Kueue controller Deployment: the image of
// its container named manager, or of its only container.
func versionOf(d *unstructured.Unstructured) ControllerVersion {
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	var image string
	for _, raw := range containers {
		c, _ := raw.(map[string]interface{})
		name, _, _ := unstructured.NestedString(c, "name")
		img, _, _ := unstructured.NestedString(c, "image")
		if name == "manager" || len(containers) == 1 {
			image = img
			break
		}
	}
	version, ok := ParseControllerVersion(image)
	if !ok {
		return ControllerVersion{}
	}
	return ControllerVersion{Version: version, Tested: VersionTested(version), Known: true}
}
