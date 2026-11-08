// Package controllerstatus publishes what the controller has actually read and
// decided, as one ConfigMap in its own namespace, so tools and operators can
// confirm a policy write without reading the controller's log.
//
// # Data contract
//
// The ConfigMap is named scaling-manager-controller-status (ConfigMapName), lives in the
// controller's namespace (POD_NAMESPACE), and carries the labels
// app.kubernetes.io/name=scaling-manager and
// app.kubernetes.io/component=controller-status. Every value is a string:
//
//	formatVersion          "1". Bumped only for an incompatible change to
//	                       these keys.
//	policyNamespace        the namespace whose scaling-policy ConfigMap is the
//	                       cluster policy: the one limiters and the optimizer
//	                       are read from.
//	policyConfigMap        that ConfigMap's name.
//	policyResourceVersion  metadata.resourceVersion of the policy ConfigMap the
//	                       controller last LOADED, accepted or rejected. Written
//	                       only after that content is applied in memory, so a
//	                       reader that wrote version V and reads V here knows
//	                       the controller is running with it. Empty when no
//	                       policy ConfigMap is in force (none exists, or it was
//	                       deleted). Compare for equality, not order:
//	                       resourceVersions are opaque.
//	policyAccepted         "true", or "false" when the ConfigMap's "default"
//	                       entry was rejected; the controller then keeps the
//	                       "default" entry previously in force, or runs
//	                       without one when there was none.
//	policyError            why the "default" entry was rejected; empty when
//	                       accepted.
//	policyIgnored          the fields of the "default" entry in the controller's
//	                       OWN scaling-policy ConfigMap that were ignored
//	                       because their value cannot be used, as
//	                       <namespace>/<name>: then each field as
//	                       <field>="<value>": <reason>, joined by "; ". Values
//	                       are cut to 64 bytes and at most 10 fields listed,
//	                       then "+<n> more". The entry still applies and those
//	                       fields act as unset. Empty when none. It describes
//	                       the entry parsed in the latest read only: when that
//	                       entry is rejected and the previous one kept, it is
//	                       empty and policyAccepted/policyError say why. That
//	                       ConfigMap is where the fields
//	                       that can be ignored (scale-to-zero retention, the
//	                       shape-change hold) are read. With a separate policy
//	                       namespace it is not the one policyConfigMap names,
//	                       and the policy namespace's own ignored fields are
//	                       not listed: they are not read there at all.
//	limiter                the limiter in force, "<mode>/<entry names>": for
//	                       example "quota/install-quota",
//	                       "inventory/gpu-inventory", or "none".
//	limiterWarnings        what is wrong with the limiters in force though
//	                       they still apply as written, joined by "; ": for
//	                       example a namespace a quota excludes that no limit
//	                       bounds. Empty when none.
//	utilizationShare       the optimizer's mode: off, invalid, shadow or
//	                       active.
//	utilizationShareError  the optimizer block's configuration error when the
//	                       mode is invalid; empty otherwise.
//	leader                 the pod that wrote this: POD_NAME, else the
//	                       hostname.
//	controllerInstance     the controller_instance metrics label, when set.
//	limiterBudgets         the GPU budget the steady-state engine used in its
//	                       last cycle, JSON (LimiterBudgets); absent until a
//	                       cycle with a limiter. See KeyLimiterBudgets.
//	clusterQuotaPerController
//	                       present while a namespace-scoped controller
//	                       enforces a scope: cluster quota: the warning
//	                       (config.ClusterQuotaPerControllerMessage). That
//	                       quota caps this controller only; absent otherwise.
//	podListDenied          present while the GPU usage walk of a
//	                       namespace-scoped controller is refused a
//	                       cluster-wide Pod list and a physical limiter reads
//	                       it: GPUs other namespaces hold on the same nodes
//	                       count as free. Mirrors llmsm_pod_access_denied;
//	                       absent otherwise.
//	utilizationShareNote   present while the optimizer's settings in force
//	                       differ from its block as written: a namespace-scoped
//	                       controller with no quota on its namespace ignores
//	                       physicalGroups and says so here. The mode is not
//	                       invalid; absent otherwise.
//	passiveDefault         "true" or "false": the LLMSM_PASSIVE this process
//	                       read at start. It is read only then, so a value
//	                       that differs from scaling-manager-manager-config means the
//	                       controller needs a restart; the installer compares
//	                       the two. Absent until set.
//	inFlightLedger         the in-flight node ledger, JSON
//	                       (allocation.PersistedInFlight): the holes held for
//	                       replicas whose pods hold no node yet, each with its
//	                       expiry. State, not status: the steady-state engine
//	                       reads it back after a restart, and a new term
//	                       carries it over. See KeyInFlightLedger.
//
// limiter, limiterWarnings and utilizationShare are recomputed from the applied configuration
// in the same update that records policyResourceVersion, so they never lag
// it. The steady-state engine republishes utilizationShare every cycle as a
// backstop; the ConfigMap is rewritten only when a value changes.
//
// # Writing
//
// Only the elected leader writes: Publisher is a manager Runnable that needs
// leader election, so a standby keeps the state in memory and writes it the
// moment it is elected. Content carries no timestamp that moves on its own
// (limiterBudgets' asOf moves only with its figures), and a write happens only
// when the content differs from what this process last wrote, so a quiet
// controller does not write at all. A failed write -- Forbidden included -- is
// logged and retried with backoff; it never stops the controller.
package controllerstatus

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// ConfigMapName is the name of the controller-status ConfigMap.
const ConfigMapName = "scaling-manager-controller-status"

// FormatVersion is the value of KeyFormatVersion this package writes.
const FormatVersion = "1"

// Data keys of the controller-status ConfigMap. See the package documentation
// for what each one means.
const (
	KeyFormatVersion         = "formatVersion"
	KeyPolicyNamespace       = "policyNamespace"
	KeyPolicyConfigMap       = "policyConfigMap"
	KeyPolicyResourceVersion = "policyResourceVersion"
	KeyPolicyAccepted        = "policyAccepted"
	KeyPolicyError           = "policyError"
	KeyPolicyIgnored         = "policyIgnored"
	KeyLimiter               = "limiter"
	KeyLimiterWarnings       = "limiterWarnings"
	KeyUtilizationShare      = "utilizationShare"
	KeyUtilizationShareError = "utilizationShareError"
	KeyLeader                = "leader"
	KeyControllerInstance    = "controllerInstance"
	// KeyBorrowReleaseLedger is the Kueue borrowing release ledger, JSON
	// (allocation.BorrowLedger): the placement-timeout holds still in force
	// and the borrowing cooldowns with their backoff, so a restart neither
	// lets a released replica grow back before its pods are gone nor forgets
	// a model's cooldown. The steady-state engine writes it and reads it back
	// on start.
	KeyBorrowReleaseLedger = "borrowReleaseLedger"
	// KeyInFlightLedger is the in-flight node ledger, JSON
	// (allocation.PersistedInFlight): the replicas ordered or woken whose
	// pods hold no node yet, with the shape and expiry of the hole each
	// holds, so a restart does not hand a held node to another decision. The
	// steady-state engine writes it and reads it back on start.
	KeyInFlightLedger = "inFlightLedger"
	// KeyClusterQuotaPerController carries the warning that a scope: cluster
	// quota caps this namespace-scoped controller only. Derived from the
	// applied configuration; absent when it does not apply.
	KeyClusterQuotaPerController = "clusterQuotaPerController"
	// KeyPodListDenied carries the warning that the GPU usage walk is refused
	// a cluster-wide Pod list while a physical limiter reads it. Absent when
	// it is not.
	KeyPodListDenied = "podListDenied"
	// KeyUtilizationShareNote carries why the optimizer's settings in force
	// differ from its block as written (config.UtilizationShare.Note), for
	// example a physicalGroups a namespace-scoped controller ignores. Absent
	// when they do not.
	KeyUtilizationShareNote = "utilizationShareNote"
	// KeyPassiveDefault is the LLMSM_PASSIVE this process read at start,
	// "true" or "false".
	KeyPassiveDefault = "passiveDefault"
)

// PodListDeniedMessage is the KeyPodListDenied value.
const PodListDeniedMessage = "this controller cannot list Pods cluster-wide, so GPU usage counts only its own " +
	"namespace (llmsm_pod_access_denied). If other namespaces run GPU pods on the same nodes, their GPUs read as " +
	"free to the physical limiter; if none do, nothing is miscounted. Grant list on pods cluster-wide, or bound " +
	"this controller with a quota limiter alone."

// persistedKeys are the keys a new term carries over from the ConfigMap it
// finds, rather than overwriting with this process's empty start: state, not
// status.
var persistedKeys = []string{KeyBorrowReleaseLedger, KeyInFlightLedger}

// Labels on the controller-status ConfigMap.
const (
	labelName      = "app.kubernetes.io/name"
	labelNameValue = "scaling-manager"
	labelComponent = "app.kubernetes.io/component"
	componentValue = "controller-status"
)

const (
	defaultDebounce = time.Second
	defaultRetryMin = 2 * time.Second
	defaultRetryMax = time.Minute
	// errorRelogEvery bounds how often an unchanged write error is logged again.
	errorRelogEvery = 5 * time.Minute
	// conflictAttempts is how many times one flush re-reads and retries after
	// a resourceVersion conflict or a create that lost a race.
	conflictAttempts = 3
	// defaultShutdownFlush bounds the last write of a term (Start).
	defaultShutdownFlush = 2 * time.Second
)

// Publisher holds the controller status and writes it to the
// controller-status ConfigMap. All methods are safe for concurrent use, and
// every setter is a no-op on a nil *Publisher, so components built without one
// (tests) need no guard.
type Publisher struct {
	writer    client.Client
	reader    client.Reader
	namespace string

	debounce time.Duration
	retryMin time.Duration
	retryMax time.Duration
	// shutdownFlush bounds the write Start makes when its context ends.
	shutdownFlush time.Duration

	mu      sync.Mutex
	desired map[string]string
	// written is the content this process last confirmed in the cluster; nil
	// until the first successful flush.
	written map[string]string
	notify  chan struct{}

	errText   string
	errLogged time.Time

	// now is the clock reportErr throttles by; tests replace it.
	now func() time.Time
}

// New returns a Publisher that writes ConfigMapName in namespace through
// writer and reads it through reader. reader should bypass the manager's
// cache (mgr.GetAPIReader()): the cache may not hold the controller's own
// namespace. A nil reader reads through writer.
func New(writer client.Client, reader client.Reader, namespace string) *Publisher {
	if reader == nil {
		reader = writer
	}
	leader := os.Getenv("POD_NAME")
	if leader == "" {
		var err error
		if leader, err = os.Hostname(); err != nil {
			ctrl.Log.WithName("controller-status").Error(err,
				"Cannot determine the hostname; the controller-status leader key will be empty. Set POD_NAME (the downward API metadata.name) to name the writer.",
				"key", KeyLeader)
		}
	}
	return &Publisher{
		writer:        writer,
		reader:        reader,
		namespace:     namespace,
		debounce:      defaultDebounce,
		retryMin:      defaultRetryMin,
		retryMax:      defaultRetryMax,
		shutdownFlush: defaultShutdownFlush,
		notify:        make(chan struct{}, 1),
		now:           time.Now,
		desired: map[string]string{
			KeyFormatVersion:         FormatVersion,
			KeyPolicyNamespace:       config.SystemNamespace(),
			KeyPolicyConfigMap:       config.ScalingPolicyConfigMapName(),
			KeyPolicyResourceVersion: "",
			KeyPolicyAccepted:        strconv.FormatBool(true),
			KeyPolicyError:           "",
			KeyPolicyIgnored:         "",
			KeyLimiter:               string(config.LimiterTypeNone),
			KeyLimiterWarnings:       "",
			KeyUtilizationShare:      constants.UtilizationShareModeOff,
			KeyUtilizationShareError: "",
			KeyLeader:                leader,
			KeyControllerInstance:    metrics.GetControllerInstance(),
		},
	}
}

// RecordPolicy records that the policy ConfigMap namespace/name at
// resourceVersion has been applied, when that ConfigMap is also the
// controller's own (no separate policy namespace). rejected is the reason its
// "default" entry was dropped (nil when it was accepted) and ignored the fields
// of that entry that were cleared as unusable (nil when none). An empty
// resourceVersion records that no policy ConfigMap is in force. It refreshes
// the values derived from cfg in the same update, so a reader never sees the
// new version beside the old limiter or optimizer mode. Call it only after the
// content is applied to cfg.
func (p *Publisher) RecordPolicy(cfg *config.Config, namespace, name, resourceVersion string,
	rejected error, ignored []config.IgnoredField) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) {
		recordPolicy(d, cfg, namespace, name, resourceVersion, rejected)
		d[KeyPolicyIgnored] = ignoredText(namespace, name, ignored)
	})
}

// RecordClusterPolicy is RecordPolicy for a policy ConfigMap in a separate
// policy namespace. It leaves policyIgnored alone: the fields that can be
// ignored are not read from that ConfigMap, so they are reported from the
// controller's own one by RecordIgnored.
func (p *Publisher) RecordClusterPolicy(cfg *config.Config, namespace, name, resourceVersion string,
	rejected error) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) {
		recordPolicy(d, cfg, namespace, name, resourceVersion, rejected)
	})
}

// RecordIgnored records the ignored fields of the "default" entry in the
// controller's own ConfigMap when a separate policy namespace holds the
// policy, and refreshes the values derived from cfg. It does not touch the
// recorded policy version: that belongs to the policy namespace's ConfigMap.
// namespace/name is the controller's own ConfigMap, the source the value names.
func (p *Publisher) RecordIgnored(cfg *config.Config, namespace, name string, ignored []config.IgnoredField) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) {
		observe(d, cfg)
		d[KeyPolicyIgnored] = ignoredText(namespace, name, ignored)
	})
}

// recordPolicy writes the policy ConfigMap's identity, version and verdict,
// and the values derived from cfg.
func recordPolicy(d map[string]string, cfg *config.Config, namespace, name, resourceVersion string, rejected error) {
	observe(d, cfg)
	d[KeyPolicyNamespace] = namespace
	d[KeyPolicyConfigMap] = name
	d[KeyPolicyResourceVersion] = resourceVersion
	d[KeyPolicyAccepted] = strconv.FormatBool(rejected == nil)
	d[KeyPolicyError] = errText(rejected)
}

// SetUtilizationShare records the optimizer's mode, one of the
// constants.UtilizationShareMode* values, and its configuration error.
func (p *Publisher) SetUtilizationShare(mode string, err error) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) {
		d[KeyUtilizationShare] = mode
		d[KeyUtilizationShareError] = errText(err)
	})
}

// SetBorrowReleaseLedger records the borrowing release ledger (JSON).
func (p *Publisher) SetBorrowReleaseLedger(ledger string) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) { d[KeyBorrowReleaseLedger] = ledger })
}

// SetInFlightLedger records the in-flight node ledger (JSON).
func (p *Publisher) SetInFlightLedger(ledger string) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) { d[KeyInFlightLedger] = ledger })
}

// ReadKey reads one key of the controller-status ConfigMap in namespace
// through reader, which should bypass the manager's cache. found is false when
// the ConfigMap or the key is absent; any other failure is returned.
func ReadKey(ctx context.Context, reader client.Reader, namespace, key string) (value string, found bool, err error) {
	var cm corev1.ConfigMap
	err = reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ConfigMapName}, &cm)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	value, found = cm.Data[key]
	return value, found, nil
}

// carryOver copies the persisted keys from the ConfigMap into the desired
// status where this process has not set them yet, so the first write of a
// term does not erase state another term persisted. A read that fails is
// returned: Start writes nothing until one succeeds (review R10), since a
// write without the persisted keys would erase them.
func (p *Publisher) carryOver(ctx context.Context) error {
	for _, key := range persistedKeys {
		v, found, err := ReadKey(ctx, p.reader, p.namespace, key)
		if err != nil {
			return fmt.Errorf("reading the persisted %s key before the first write: %w", key, err)
		}
		if !found {
			continue
		}
		p.mu.Lock()
		if _, set := p.desired[key]; !set {
			p.desired[key] = v
		}
		p.mu.Unlock()
	}
	return nil
}

// Data returns a copy of the status as it will be written.
func (p *Publisher) Data() map[string]string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.desired)
}

// NeedLeaderElection reports that only the elected leader writes the status.
func (p *Publisher) NeedLeaderElection() bool { return true }

// Start writes the status, then rewrites it whenever it changes, until ctx is
// done. Changes arriving within the debounce interval are coalesced into one
// write. A failed write is logged and retried with backoff; Start never
// returns an error, because a status the controller cannot publish is no
// reason to stop scaling.
func (p *Publisher) Start(ctx context.Context) error {
	logger := ctrl.Log.WithName("controller-status")
	// A new term compares against the cluster, not against what an earlier
	// term of this process wrote: another leader may have written since.
	p.mu.Lock()
	p.written = nil
	// Read here, not in New: main builds the Publisher before InitMetrics
	// reads the instance from the environment.
	p.desired[KeyControllerInstance] = metrics.GetControllerInstance()
	p.mu.Unlock()
	carried := false
	p.signal() // the first write after election, whatever is pending
	backoff := p.retryMin
	// One timer for the whole term, armed only while a write is failing. retry
	// is nil while disarmed, so a tick can never be received then; and Stop and
	// Reset (Go 1.23+) discard a tick that fired but was not received, so a
	// re-armed timer never delivers the previous attempt's tick early.
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			p.flushOnShutdown(ctx, logger, carried)
			return nil
		case <-p.notify:
		case <-retry:
		}
		if !sleep(ctx, p.debounce) {
			p.flushOnShutdown(ctx, logger, carried)
			return nil
		}
		err := error(nil)
		if !carried {
			// The persisted keys first, retried with the write's backoff.
			if err = p.carryOver(ctx); err == nil {
				carried = true
			}
		}
		if err == nil {
			err = p.flush(ctx)
		}
		if err != nil {
			p.reportErr(logger, err)
			timer.Reset(backoff)
			retry = timer.C
			backoff = min(2*backoff, p.retryMax)
			continue
		}
		p.reportRecovered(logger)
		timer.Stop()
		retry, backoff = nil, p.retryMin
	}
}

// flushOnShutdown writes what changed since the last write once more when the
// term ends, bounded by shutdownFlush: a change the debounce still held --
// the in-flight ledger's last cycle, say -- would otherwise be lost to the
// next leader. Only after the persisted keys were carried over: a write
// without them would erase them. A failure is logged and dropped.
func (p *Publisher) flushOnShutdown(ctx context.Context, logger logr.Logger, carried bool) {
	if !carried {
		return
	}
	// The old leader's conflict retry cannot overwrite a new leader's write: it
	// stops leading at RenewDeadline (50s by default), before another can take
	// the lease at LeaseDuration (60s), and this write is capped at shutdownFlush (2s).
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.shutdownFlush)
	defer cancel()
	if err := p.flush(fctx); err != nil {
		logger.Info("Could not write the controller status on shutdown; the last change is lost to the next leader",
			"error", err.Error())
	}
}

// update applies mutate to the desired status and wakes the writer when it
// changed anything.
func (p *Publisher) update(mutate func(map[string]string)) {
	p.mu.Lock()
	before := maps.Clone(p.desired)
	mutate(p.desired)
	changed := !maps.Equal(before, p.desired)
	p.mu.Unlock()
	if changed {
		p.signal()
	}
}

// signal wakes Start without blocking; one pending wake covers any number.
func (p *Publisher) signal() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// flush writes the desired status unless it is what this process last wrote.
// It returns every API error but conflicts it could resolve by re-reading; a
// Forbidden is returned like any other, never treated as nothing to do, and
// the desired status is kept for the next attempt.
func (p *Publisher) flush(ctx context.Context) error {
	p.mu.Lock()
	want := maps.Clone(p.desired)
	same := p.written != nil && maps.Equal(want, p.written)
	p.mu.Unlock()
	if same {
		return nil
	}
	var err error
	for range conflictAttempts {
		if err = p.writeOnce(ctx, want); err == nil {
			p.mu.Lock()
			p.written = want
			p.mu.Unlock()
			return nil
		}
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return err
}

// writeOnce reads the ConfigMap and creates it, or updates it at the version
// it read, when its data or labels differ from want.
func (p *Publisher) writeOnce(ctx context.Context, want map[string]string) error {
	var cm corev1.ConfigMap
	err := p.reader.Get(ctx, client.ObjectKey{Namespace: p.namespace, Name: ConfigMapName}, &cm)
	if apierrors.IsNotFound(err) {
		return p.writer.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ConfigMapName,
				Namespace: p.namespace,
				Labels:    map[string]string{labelName: labelNameValue, labelComponent: componentValue},
			},
			Data: maps.Clone(want),
		})
	}
	if err != nil {
		return err
	}
	if maps.Equal(cm.Data, want) && cm.Labels[labelName] == labelNameValue && cm.Labels[labelComponent] == componentValue {
		return nil
	}
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels[labelName] = labelNameValue
	cm.Labels[labelComponent] = componentValue
	cm.Data = maps.Clone(want)
	return p.writer.Update(ctx, &cm)
}

// reportErr logs a write failure at Error level when its text changes, and an
// unchanged one again only after errorRelogEvery, so a standing Forbidden is
// visible without flooding the log.
func (p *Publisher) reportErr(logger logr.Logger, err error) {
	p.mu.Lock()
	text := err.Error()
	now := p.now()
	relog := text != p.errText || now.Sub(p.errLogged) >= errorRelogEvery
	p.errText = text
	if relog {
		p.errLogged = now
	}
	p.mu.Unlock()
	if !relog {
		return
	}
	kv := []any{"configMap", ConfigMapName, "namespace", p.namespace}
	if apierrors.IsForbidden(err) {
		kv = append(kv, "hint", "the controller's ServiceAccount needs the scaling-manager-controller-status-role Role (get/update on this ConfigMap, create on configmaps)")
	}
	logger.Error(err, "Failed to write the controller-status ConfigMap; retrying", kv...)
	metrics.RecordError(constants.ComponentController, "Failed to write controller status")
}

// reportRecovered logs once that a write succeeded after failing.
func (p *Publisher) reportRecovered(logger logr.Logger) {
	p.mu.Lock()
	failed := p.errText != ""
	p.errText = ""
	p.mu.Unlock()
	if failed {
		logger.Info("Controller-status ConfigMap written after earlier failures",
			"configMap", ConfigMapName, "namespace", p.namespace)
	}
}

// observe sets the values derived from cfg.
func observe(d map[string]string, cfg *config.Config) {
	if cfg == nil {
		return
	}
	d[KeyPolicyNamespace] = cfg.PolicyNamespace()
	d[KeyLimiter] = cfg.EffectiveLimiterSummary()
	d[KeyLimiterWarnings] = strings.Join(cfg.EffectiveLimiterWarnings(), "; ")
	mode, err := cfg.UtilizationShareMode()
	d[KeyUtilizationShare] = mode
	d[KeyUtilizationShareError] = errText(err)
	setOrDelete(d, KeyUtilizationShareNote, cfg.UtilizationShareNote())
	if cfg.ClusterQuotaPerController() {
		d[KeyClusterQuotaPerController] = config.ClusterQuotaPerControllerMessage
	} else {
		delete(d, KeyClusterQuotaPerController)
	}
}

// SetPodListDenied records whether the GPU usage walk is refused a
// cluster-wide Pod list while a physical limiter reads it.
func (p *Publisher) SetPodListDenied(denied bool) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) {
		if denied {
			d[KeyPodListDenied] = PodListDeniedMessage
		} else {
			delete(d, KeyPodListDenied)
		}
	})
}

// SetUtilizationShareNote records why the optimizer's settings in force differ
// from its block as written; "" removes the key.
func (p *Publisher) SetUtilizationShareNote(note string) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) { setOrDelete(d, KeyUtilizationShareNote, note) })
}

// SetPassiveDefault records the LLMSM_PASSIVE this process read at start.
func (p *Publisher) SetPassiveDefault(passive bool) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) { d[KeyPassiveDefault] = strconv.FormatBool(passive) })
}

// setOrDelete sets key to value, or removes it when value is empty.
func setOrDelete(d map[string]string, key, value string) {
	if value == "" {
		delete(d, key)
		return
	}
	d[key] = value
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ignoredText is the policyIgnored value: the source ConfigMap, then each
// field. Prefixed with its source because it sits beside the policy* keys,
// which name the policy namespace's ConfigMap when one is separate, and this
// is always the controller's own. Values are already truncated
// (config.TruncateIgnoredValue); at most maxIgnoredFieldsListed are listed.
func ignoredText(namespace, name string, ignored []config.IgnoredField) string {
	if len(ignored) == 0 {
		return ""
	}
	shown := ignored[:min(len(ignored), maxIgnoredFieldsListed)]
	parts := make([]string, 0, len(shown)+1)
	for _, f := range shown {
		parts = append(parts, f.String())
	}
	if more := len(ignored) - len(shown); more > 0 {
		parts = append(parts, fmt.Sprintf("+%d more", more))
	}
	return namespace + "/" + name + ": " + strings.Join(parts, "; ")
}

// maxIgnoredFieldsListed caps the fields policyIgnored lists.
const maxIgnoredFieldsListed = 10

// sleep waits d or until ctx is done, reporting whether the full wait elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
