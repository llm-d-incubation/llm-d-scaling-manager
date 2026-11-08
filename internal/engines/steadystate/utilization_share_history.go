package steadystate

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// The utilization-share need history survives a restart in one ConfigMap in
// the controller's namespace, one data key per group. Without it a restarted
// ledger knows no model's peak, and the planner takes every peak at its
// ceiling -- the headroom cap on -- for a whole swing window. Metrics cannot
// be the read-back store: they carry no writer and no honest age.
//
// Only the acting (leader) path writes it: each group's history is staged
// when it changed, at most once per shareHistoryInterval, and every staged
// group is written in one read-modify-write per cycle, with the ConfigMap's
// resourceVersion as the concurrency check. Every ledger built in a cycle
// restores from one read. Deleting the ConfigMap flushes the history: a
// controller that has seen it and then finds it gone forgets its in-memory
// history too, rather than writing it straight back.
const (
	// shareHistoryConfigMap is the ConfigMap's name. The controller's Role
	// (config/base/rbac/utilization-share-history-role.yaml) grants get and
	// update on this name only.
	shareHistoryConfigMap = "scaling-manager-utilization-share-history"
	// shareHistoryVersion is the stored format's version. An entry of any
	// other version is ignored, not guessed at, and pruned once older than
	// shareHistoryForeignHorizon.
	shareHistoryVersion = 1
	// shareHistoryInterval is the least time between two writes of a group's
	// history, and between two reads retried after a failed one.
	shareHistoryInterval = time.Minute
	// shareHistoryMaxRoles is the most roles one group's entry stores.
	shareHistoryMaxRoles = 128
	// shareHistoryMaxBytes bounds the whole ConfigMap's data, below the API
	// server's 1 MiB object limit with room for its metadata. Each group
	// written gets an equal share of it.
	shareHistoryMaxBytes = 900 << 10
	// shareHistoryForeignHorizon is how long an entry of another format
	// version is kept: a controller rolled back to it may still read it.
	shareHistoryForeignHorizon = 7 * 24 * time.Hour
	// shareHistoryDigits is the significant digits a stored need keeps:
	// a need is a replica count's fraction, and full float64 precision
	// tripled the entry's size for nothing.
	shareHistoryDigits = 4
)

// errShareHistoryTooLarge is a write refused because the ConfigMap would
// outgrow shareHistoryMaxBytes.
var errShareHistoryTooLarge = errors.New("the utilization-share history ConfigMap would exceed its size bound")

// errShareHistoryDeleted is a write that found the ConfigMap deleted since it
// was read: an operator flushed the history.
var errShareHistoryDeleted = errors.New("the utilization-share history ConfigMap was deleted")

// shareHistoryEntry is one group's stored need history.
type shareHistoryEntry struct {
	Version int `json:"version"`
	// Group is the unsanitized group key: two groups whose keys sanitize
	// alike never read each other's history.
	Group     string    `json:"group"`
	WrittenAt time.Time `json:"writtenAt"`
	// SwingWindow is the window the history was kept over when written. An
	// entry is expired, and removed by the next write, once all of it lies
	// further back than this.
	SwingWindow        metav1.Duration             `json:"swingWindow"`
	ControllerInstance string                      `json:"controllerInstance,omitempty"`
	Roles              map[string]shareHistoryRole `json:"roles"`
}

// shareHistoryRole is a role's stored history: since when its need has been
// recorded, and its sustained need per bucket as [unix seconds, need].
type shareHistoryRole struct {
	Since   time.Time    `json:"since"`
	Samples [][2]float64 `json:"samples"`
}

// peak is the highest need the role stores: what decides, when an entry is
// over its bounds, which roles are left out first.
func (r shareHistoryRole) peak() float64 {
	p := 0.0
	for _, s := range r.Samples {
		p = math.Max(p, s[1])
	}
	return p
}

// shareHistoryState is the store's memory between cycles. It is not reset
// with the ledgers: the ConfigMap outlives them.
type shareHistoryState struct {
	// cm is the ConfigMap as last read or written; nil when it must be read
	// before the next write (never read, absent, or a write conflicted).
	cm *corev1.ConfigMap
	// seen is set once this controller has read the ConfigMap or written it,
	// and cleared by a flush: from then on, finding it absent means an
	// operator deleted it.
	seen bool
	// readAt is the cycle whose read readCM and readErr are: every ledger
	// built in one cycle restores from the same read.
	readAt  time.Time
	readCM  *corev1.ConfigMap
	readErr error
	// unread are the groups whose history could not be read when their
	// ledger was built, by when the read was last tried. Their history is
	// neither restored nor written until a read succeeds: a write would
	// replace the history it could not read.
	unread map[string]time.Time
	// lastWrite is when each group's history was last staged for a write,
	// and written what it was, so an unchanged history is not written again.
	lastWrite map[string]time.Time
	written   map[string]string
	// pending are this cycle's staged entries, written together.
	pending map[string]shareHistoryPending
	// errs is each operation's last error text per group, so a standing
	// failure is logged once per change and a recovery once.
	errs map[string]string
	// dropped is the roles each group's last write left out, so a
	// truncation is logged once per change.
	dropped map[string]string
}

// shareHistoryPending is a group's history staged for this cycle's write.
type shareHistoryPending struct {
	entry   shareHistoryEntry
	content string
	// dropped are the roles left out past shareHistoryMaxRoles.
	dropped []string
}

// shareHistoryKey is the ConfigMap data key of a group's history: the
// controller instance (CONTROLLER_INSTANCE) and the group key, with every
// character a data key may not hold replaced, and a hash of the original, so
// distinct groups -- or two instances sharing a namespace and a group -- never
// share a key.
func shareHistoryKey(group string) string {
	id := group
	if inst := metrics.GetControllerInstance(); inst != "" {
		id = inst + "/" + group
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 200 {
		s = s[:200]
	}
	sum := sha256.Sum256([]byte(id))
	return s + "." + hex.EncodeToString(sum[:4])
}

// shareHistoryReader reads the ConfigMap uncached: the manager's cache holds
// only labelled ConfigMaps, or only the watched namespace, and either may
// exclude the controller's own.
func (e *Engine) shareHistoryReader() client.Reader {
	if e.apiReader != nil {
		return e.apiReader
	}
	return e.client
}

// reportShareHistoryErr logs a store operation's failure for a group at Error
// level once per change of its error, and its recovery once: only when that
// group's operation succeeds after failing.
func (st *utilizationShareState) reportShareHistoryErr(logger logr.Logger, op, group string, err error, msg string) {
	h := &st.history
	if h.errs == nil {
		h.errs = map[string]string{}
	}
	text := ""
	if err != nil {
		text = err.Error()
	}
	key := op + "|" + group
	if text == h.errs[key] {
		return
	}
	prev := h.errs[key]
	if text == "" {
		delete(h.errs, key)
	} else {
		h.errs[key] = text
	}
	kv := []any{"group", group, "configMap", shareHistoryConfigMap, "namespace", config.SystemNamespace()}
	if err != nil {
		logger.Error(err, msg, kv...)
		return
	}
	if prev != "" {
		logger.Info("Utilization share: the need history store works again", append(kv, "operation", op)...)
	}
}

// readShareHistory reads the ConfigMap. An absent ConfigMap is no history: it
// returns nil and no error. Any other failure -- Forbidden included -- is an
// error, never an empty history.
func (e *Engine) readShareHistory(ctx context.Context) (*corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	err := e.shareHistoryReader().Get(ctx, client.ObjectKey{Namespace: config.SystemNamespace(), Name: shareHistoryConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cm, nil
}

// readShareHistoryCycle is this cycle's read of the ConfigMap, shared by every
// ledger built in it. Finding the ConfigMap absent after this controller has
// seen it is the flush (flushShareHistory).
func (e *Engine) readShareHistoryCycle(ctx context.Context, logger logr.Logger, now time.Time) (*corev1.ConfigMap, error) {
	h := &e.utilizationShare.history
	if !h.readAt.IsZero() && h.readAt.Equal(now) {
		return h.readCM, h.readErr
	}
	cm, err := e.readShareHistory(ctx)
	h.readAt, h.readCM, h.readErr = now, cm, err
	if err != nil {
		return nil, err
	}
	if cm == nil && h.seen {
		e.flushShareHistory(logger)
	}
	h.cm, h.seen = cm, h.seen || cm != nil
	return cm, nil
}

// flushShareHistory forgets every ledger's need history, as an operator who
// deleted the ConfigMap asked: rewritten from memory, the delete would undo
// nothing. Every peak is unknown again for a window, as after a restart with
// no store; the next write creates the ConfigMap afresh.
func (e *Engine) flushShareHistory(logger logr.Logger) {
	st := &e.utilizationShare
	for _, l := range st.ledgers {
		l.ForgetNeedHistory()
	}
	clear(st.history.written)
	clear(st.history.lastWrite)
	clear(st.history.pending)
	st.history.cm, st.history.seen, st.history.readCM = nil, false, nil
	logger.Info("Utilization share: the need history ConfigMap was deleted; every model's need history is forgotten",
		"configMap", shareHistoryConfigMap, "namespace", config.SystemNamespace())
}

// restoreShareHistory reads a group's stored need history into its new ledger,
// before its first plan. On a failed read nothing is restored -- the ledger
// starts as it did without a store, its peaks unknown for a window -- the
// failure is logged, and the read is retried (stageShareHistory) until it
// succeeds.
func (e *Engine) restoreShareHistory(ctx context.Context, logger logr.Logger, group string, l *allocation.ShareLedger,
	now time.Time, tm allocation.ShareTimings) {
	st := &e.utilizationShare
	if e.client == nil {
		return
	}
	if st.history.unread == nil {
		st.history.unread = map[string]time.Time{}
	}
	cm, err := e.readShareHistoryCycle(ctx, logger, now)
	st.reportShareHistoryErr(logger, "read", group, err,
		"Utilization share: could not read the stored need history; the models' peaks are unknown for a swing window "+
			"and their headroom is capped meanwhile. Retrying every minute; nothing is written until a read succeeds")
	if err != nil {
		st.history.unread[group] = now
		return
	}
	delete(st.history.unread, group)
	if cm == nil {
		return
	}
	raw, ok := cm.Data[shareHistoryKey(group)]
	if !ok {
		return
	}
	h, entry, why := decodeShareHistory(raw, group)
	if why != "" {
		logger.Info("Utilization share: stored need history not used", "reason", why, "group", group)
		return
	}
	if entry.SwingWindow.Duration < tm.SwingWindow {
		// Kept over a shorter window than today's: the samples cover the
		// old window only, so no role's peak is known until today's is
		// covered -- from its oldest sample, not from its Since.
		for r, rh := range h {
			rh.Since = time.Time{}
			h[r] = rh
		}
	}
	res := l.RestoreNeedHistory(h, now, tm)
	if res.Roles == 0 {
		logger.Info("Utilization share: stored need history not used", "reason", "older than its window",
			"group", group, "writtenAt", entry.WrittenAt, "swingWindow", tm.SwingWindow)
		return
	}
	kv := []any{"group", group, "roles", res.Roles, "rolesKnownNow", res.KnownNow,
		"writtenAt", entry.WrittenAt, "writtenBy", entry.ControllerInstance}
	if res.KnownAt.IsZero() {
		kv = append(kv, "peaksKnownAt", fmt.Sprintf("a swing window after recording resumes: the history ends more than %v ago",
			allocation.ShareHistoryMaxGap))
	} else {
		kv = append(kv, "peaksKnownAt", res.KnownAt)
	}
	logger.Info("Utilization share: need history restored", kv...)
}

// decodeShareHistory parses a stored entry. why is non-empty when the entry
// is not used at all.
func decodeShareHistory(raw, group string) (h allocation.ShareNeedHistory, entry shareHistoryEntry, why string) {
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return nil, entry, "unparseable: " + err.Error()
	}
	if entry.Version != shareHistoryVersion {
		return nil, entry, fmt.Sprintf("unknown version %d", entry.Version)
	}
	if entry.Group != group || entry.ControllerInstance != metrics.GetControllerInstance() {
		return nil, entry, fmt.Sprintf("written for group %q of controller instance %q", entry.Group, entry.ControllerInstance)
	}
	roles := shareHistoryRolesByPeak(entry.Roles)
	if len(roles) > shareHistoryMaxRoles {
		roles = roles[:shareHistoryMaxRoles]
	}
	h = make(allocation.ShareNeedHistory, len(roles))
	for _, r := range roles {
		stored := entry.Roles[r]
		if len(stored.Samples) > allocation.ShareHistoryMaxSamples {
			stored.Samples = stored.Samples[len(stored.Samples)-allocation.ShareHistoryMaxSamples:]
		}
		rh := allocation.ShareRoleNeedHistory{Since: stored.Since, Samples: make([]allocation.ShareNeedSample, 0, len(stored.Samples))}
		for _, s := range stored.Samples {
			rh.Samples = append(rh.Samples, allocation.ShareNeedSample{At: time.Unix(int64(s[0]), 0), Need: s[1]})
		}
		h[r] = rh
	}
	return h, entry, ""
}

// shareHistoryRolesByPeak is roles' keys, highest stored peak first (ties by
// key): the order in which they are kept when an entry is over its bounds. A
// role left out counts at its ceiling after a restart; the highest peaks are
// the ones a ceiling would overstate least, but they are also the ones that
// decide whether the peaks fit, so the smallest are left out first.
func shareHistoryRolesByPeak(roles map[string]shareHistoryRole) []string {
	keys := slices.Collect(maps.Keys(roles))
	peaks := make(map[string]float64, len(keys))
	for _, k := range keys {
		peaks[k] = roles[k].peak()
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(peaks[b], peaks[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return keys
}

// shareHistoryRound rounds a need up to shareHistoryDigits significant
// digits: never below what was recorded, so a stored peak is never lower.
func shareHistoryRound(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(v, 'e', shareHistoryDigits-1, 64), 64)
	if err != nil {
		return v
	}
	if r < v {
		exp := math.Floor(math.Log10(math.Abs(r)))
		r, _ = strconv.ParseFloat(strconv.FormatFloat(r+math.Pow(10, exp-shareHistoryDigits+1), 'e', shareHistoryDigits-1, 64), 64)
	}
	return r
}

// newShareHistoryEntry is a group's entry, its WrittenAt not set, keeping at
// most shareHistoryMaxRoles roles -- the smallest peaks are left out -- and
// the roles it left out.
func newShareHistoryEntry(group string, h allocation.ShareNeedHistory, tm allocation.ShareTimings) (shareHistoryEntry, []string) {
	entry := shareHistoryEntry{
		Version: shareHistoryVersion, Group: group,
		SwingWindow: metav1.Duration{Duration: tm.SwingWindow}, ControllerInstance: metrics.GetControllerInstance(),
		Roles: map[string]shareHistoryRole{},
	}
	for r, rh := range h {
		if len(rh.Samples) == 0 {
			continue
		}
		sr := shareHistoryRole{Since: rh.Since.UTC(), Samples: make([][2]float64, 0, len(rh.Samples))}
		for _, s := range rh.Samples {
			sr.Samples = append(sr.Samples, [2]float64{float64(s.At.Unix()), shareHistoryRound(s.Need)})
		}
		entry.Roles[r] = sr
	}
	var dropped []string
	if roles := shareHistoryRolesByPeak(entry.Roles); len(roles) > shareHistoryMaxRoles {
		dropped = roles[shareHistoryMaxRoles:]
		for _, r := range dropped {
			delete(entry.Roles, r)
		}
	}
	return entry, dropped
}

// shareHistoryContent is what decides whether an entry changed: everything but
// when it was written.
func shareHistoryContent(entry shareHistoryEntry) string {
	entry.WrittenAt = time.Time{}
	b, _ := json.Marshal(entry)
	return string(b)
}

// encodeShareHistory is entry, written at now, within budget bytes: while it
// is over, the role with the smallest stored peak is left out. It returns the
// roles it left out.
func encodeShareHistory(entry shareHistoryEntry, now time.Time, budget int) (raw string, dropped []string, err error) {
	entry.WrittenAt = now.UTC()
	entry.Roles = maps.Clone(entry.Roles)
	b, err := json.Marshal(entry)
	if err != nil {
		return "", nil, err
	}
	if len(b) <= budget {
		return string(b), nil, nil
	}
	// Estimate each role's share, drop the smallest peaks until the estimate
	// fits, then re-check the real size.
	roles := shareHistoryRolesByPeak(entry.Roles)
	size := len(b)
	for len(roles) > 0 && size > budget {
		r := roles[len(roles)-1]
		roles = roles[:len(roles)-1]
		rb, _ := json.Marshal(map[string]shareHistoryRole{r: entry.Roles[r]})
		size -= len(rb) - 1 // the braces, less the comma that separated it
		delete(entry.Roles, r)
		dropped = append(dropped, r)
		if size <= budget {
			if b, err = json.Marshal(entry); err != nil {
				return "", nil, err
			}
			size = len(b)
		}
	}
	if size > budget {
		return "", dropped, fmt.Errorf("%w: one group's entry is %d bytes with no role left, over its %d", errShareHistoryTooLarge, size, budget)
	}
	return string(b), dropped, nil
}

// shareHistoryStale reports whether a stored entry is to be removed by the
// next write. An entry of this version is, once written longer ago than its
// swing window (and ShareHistoryMaxGap): nothing a restore could use is left.
// One of another version is kept for shareHistoryForeignHorizon, for a
// controller rolled back to read; one that is not an entry at all, or holds
// no writtenAt to age it by, at once.
func shareHistoryStale(raw string, now time.Time) bool {
	var head struct {
		Version   int       `json:"version"`
		WrittenAt time.Time `json:"writtenAt"`
	}
	if json.Unmarshal([]byte(raw), &head) != nil || head.WrittenAt.IsZero() {
		return true
	}
	if head.Version != shareHistoryVersion {
		return now.Sub(head.WrittenAt) > shareHistoryForeignHorizon
	}
	var entry shareHistoryEntry
	if json.Unmarshal([]byte(raw), &entry) != nil {
		return true
	}
	return now.Sub(entry.WrittenAt) > entry.SwingWindow.Duration+allocation.ShareHistoryMaxGap
}

// stageShareHistory stages a group's need history for this cycle's write
// (writeShareHistory) when it changed, at most once per shareHistoryInterval.
// It runs only on the acting path: shadow mode keeps no ledger and writes
// nothing. A group whose history could not be read retries the read instead,
// and writes nothing until it succeeds.
func (e *Engine) stageShareHistory(ctx context.Context, logger logr.Logger, group string, tm allocation.ShareTimings, now time.Time) {
	st := &e.utilizationShare
	l := st.ledgers[group]
	if e.client == nil || l == nil {
		return
	}
	if tried, unread := st.history.unread[group]; unread {
		if now.Sub(tried) >= shareHistoryInterval {
			e.restoreShareHistory(ctx, logger, group, l, now, tm)
		}
		return
	}
	if at, ok := st.history.lastWrite[group]; ok && now.Sub(at) < shareHistoryInterval {
		return
	}
	entry, dropped := newShareHistoryEntry(group, l.NeedHistory(now, tm), tm)
	content := shareHistoryContent(entry)
	if st.history.written[group] == content {
		return
	}
	if st.history.pending == nil {
		st.history.pending = map[string]shareHistoryPending{}
	}
	st.history.pending[group] = shareHistoryPending{entry: entry, content: content, dropped: dropped}
}

// writeShareHistory writes every group staged this cycle in one
// read-modify-write of the ConfigMap, each group's entry within an equal
// share of shareHistoryMaxBytes, and removes the stale entries
// (shareHistoryStale) on the way.
func (e *Engine) writeShareHistory(ctx context.Context, logger logr.Logger, now time.Time) {
	st := &e.utilizationShare
	pending := st.history.pending
	st.history.pending = nil
	if e.client == nil || len(pending) == 0 {
		return
	}
	if st.history.lastWrite == nil {
		st.history.lastWrite, st.history.written, st.history.dropped = map[string]time.Time{}, map[string]string{}, map[string]string{}
	}
	keys := make(map[string]string, len(pending))
	for g := range pending {
		keys[shareHistoryKey(g)] = g
		st.history.lastWrite[g] = now
	}
	dropped := map[string][]string{}
	encodeErrs := map[string]error{}
	err := e.updateShareHistory(ctx, func(data map[string]string) bool {
		clear(dropped)
		clear(encodeErrs)
		for k, v := range data {
			if _, ours := keys[k]; !ours && shareHistoryStale(v, now) {
				delete(data, k)
			}
		}
		groups := len(data)
		for k := range keys {
			if _, ok := data[k]; !ok {
				groups++
			}
		}
		budget := shareHistoryMaxBytes / max(groups, 1)
		for k, g := range keys {
			p := pending[g]
			raw, more, err := encodeShareHistory(p.entry, now, budget-len(k))
			dropped[g] = append(slices.Clone(p.dropped), more...)
			if err != nil {
				encodeErrs[g] = err
				continue
			}
			data[k] = raw
		}
		return true
	})
	if errors.Is(err, errShareHistoryDeleted) {
		e.flushShareHistory(logger)
		return
	}
	for g, p := range pending {
		gerr := err
		if gerr == nil {
			gerr = encodeErrs[g]
		}
		st.reportShareHistoryErr(logger, "write", g, gerr,
			"Utilization share: could not store the need history; a restart now forgets the models' peaks")
		if gerr != nil {
			continue
		}
		st.history.written[g] = p.content
		if d := strings.Join(dropped[g], ","); d != st.history.dropped[g] {
			st.history.dropped[g] = d
			if d != "" {
				logger.Info("Utilization share: need history truncated; the roles left out count at their ceiling after a restart",
					"group", g, "droppedRoles", len(dropped[g]), "dropped", shareHistoryNames(dropped[g]),
					"maxRoles", shareHistoryMaxRoles, "maxBytes", shareHistoryMaxBytes)
			}
		}
	}
}

// shareHistoryNames is at most the first ten of names, for a log line.
func shareHistoryNames(names []string) string {
	if len(names) <= 10 {
		return strings.Join(names, ",")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:10], ","), len(names)-10)
}

// forgetShareHistory drops what the controller remembers of a vanished
// group's stored history. The stored entry stays: a group dropped after a
// few minutes of failed collection comes back with its peaks (a restore needs
// samples within ShareHistoryMaxGap of now), and an entry nothing reads any
// more is pruned once older than its window (shareHistoryStale).
func (st *utilizationShareState) forgetShareHistory(group string) {
	delete(st.history.unread, group)
	delete(st.history.lastWrite, group)
	delete(st.history.written, group)
	delete(st.history.dropped, group)
	delete(st.history.pending, group)
	for _, op := range []string{"read", "write"} {
		delete(st.history.errs, op+"|"+group)
	}
}

// updateShareHistory applies mutate to the ConfigMap's data and writes it,
// creating the ConfigMap when absent. The write carries the resourceVersion it
// was based on; on a conflict -- another writer -- it reads again and retries
// once, never more. The ConfigMap found absent, by the read or the write,
// after this controller has seen it is the flush: errShareHistoryDeleted.
func (e *Engine) updateShareHistory(ctx context.Context, mutate func(map[string]string) bool) error {
	st := &e.utilizationShare
	var err error
	for range 2 {
		cm := st.history.cm
		if cm == nil {
			if cm, err = e.readShareHistory(ctx); err != nil {
				return err
			}
			if cm == nil && st.history.seen {
				return errShareHistoryDeleted
			}
			st.history.seen = st.history.seen || cm != nil
		}
		var obj *corev1.ConfigMap
		if cm != nil {
			obj = cm.DeepCopy()
		} else {
			obj = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: shareHistoryConfigMap, Namespace: config.SystemNamespace(),
				Labels: map[string]string{"app.kubernetes.io/component": "utilization-share-history",
					"app.kubernetes.io/part-of": "scaling-manager"},
			}}
		}
		if obj.Data == nil {
			obj.Data = map[string]string{}
		}
		if !mutate(obj.Data) {
			st.history.cm = cm
			return nil
		}
		size := 0
		for k, v := range obj.Data {
			size += len(k) + len(v)
		}
		if size > shareHistoryMaxBytes {
			st.history.cm = cm
			return fmt.Errorf("%w of %d bytes", errShareHistoryTooLarge, shareHistoryMaxBytes)
		}
		if obj.ResourceVersion == "" {
			err = e.client.Create(ctx, obj)
		} else {
			err = e.client.Update(ctx, obj)
		}
		if err == nil {
			st.history.cm, st.history.seen = obj, true
			return nil
		}
		st.history.cm = nil
		if apierrors.IsNotFound(err) && obj.ResourceVersion != "" {
			// Deleted since it was read: the flush (writeShareHistory).
			return errShareHistoryDeleted
		}
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return err
}
