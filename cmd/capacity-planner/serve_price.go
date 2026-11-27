package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The capacity planner's API (whatifapi) prices on the CLI's own code paths:
// a scenario's fleet is the fleet slo-to-rate and replicas-to-slo would read
// with the flags its knobs stand for, and each figure is the one those
// commands print. This file is that wiring; it adds no arithmetic.

// defaultWindow is a Prometheus source's window when the request names none:
// the last hour, ending now.
const defaultWindow = time.Hour

// servedSources resolves inputs from the server's sources: its Prometheus,
// read with capture's flags, and its recordings directory.
type servedSources struct {
	window     captureFlags
	recordings string
	warn       io.Writer
	// now is the clock a default window ends at; time.Now when nil.
	now func() time.Time
	// allowed is the namespaces a prometheus source may read, and
	// namespaceAllowed, with --auth=kubernetes, whether the request's user may
	// read one (whatifapi.Authenticator.NamespaceAllowed); nil with no auth.
	allowed          []string
	namespaceAllowed func(ctx context.Context, namespace string) error
	// uploads is the recordings uploaded (--allow-upload), nil with none;
	// maxUploadWeight bounds what one of them may keep loaded, in the
	// weight the inputs cache counts (--inputs-cache-size).
	uploads         *uploadStore
	maxUploadWeight int
	// experimentalTTFT is serve's --experimental-ttft: every source is read
	// with the experimental TTFT model, as if each request asked for it.
	experimentalTTFT bool
	// rankMixture and tpotDistribution are serve's --experimental-ttft=v2
	// and --experimental-tpot=v2: every source also gives the cycles the
	// experimental backtest models read (experimentalSeries).
	rankMixture, tpotDistribution bool
	// replay is serve's opt-in replay switches, each on for every source
	// (serve_replay.go).
	replay replayDefaults
	// onPass, when set, is called as each pass over a recording file opens,
	// with the pass's count (recordingSource.onPass): a test's hook.
	onPass func(pass int)
	// onOpen, when set, is called with a recording's path after the path is
	// checked and before the file is opened: a test's hook to swap the file.
	onOpen func(path string)
}

// apiError is a CLI error as the API's: a usage or input error is the
// caller's (400), anything else the server's.
func apiError(err error) error {
	var u usageError
	if errors.As(err, &u) {
		return whatifapi.InputError{Err: err}
	}
	return err
}

// Resolve reads the source, replays it to the cycle, and reads the fleet at
// the recorded counts as the CLI would, the TTFT at the policy's statistic.
func (s *servedSources) Resolve(ctx context.Context, req whatifapi.InputsRequest) (*whatifapi.Resolved, error) {
	f := answerFlags{question: "serve", cycle: "last", model: req.Model, variant: req.DecodeVariant, prefillVariant: req.PrefillVariant,
		serveSelfCheck: req.SelfCheck != nil && *req.SelfCheck,
		seed:           whatif.DefaultResampling().Seed, vpPlateau: req.PrefillVPPlateau, hitAtCount: req.PrefillHitAtCount,
		vpServiceLine: req.PrefillVPServiceLine, fLoadCurve: req.PrefillFLoadCurve,
		experimentalTTFT: req.ExperimentalTTFT || s.experimentalTTFT, set: map[string]bool{}, warn: s.warn, priceRouterWait: whatif.DefaultPriceRouterWait,
		routerWaitInPrefill: whatif.DefaultRouterWaitInPrefill, psOnsets: whatif.DefaultPSOnsets, chargeDecodeGate: whatif.DefaultChargeDecodeGate, imbalanceAcrossCounts: whatif.DefaultPlannerImbalanceAcrossCounts, loadAwareRouter: whatif.DefaultPlannerLoadAwareRouter}

	s.replay.apply(&f)
	src := whatifapi.SourceRequest{Kind: whatifapi.SourcePrometheus}
	if req.Source != nil {
		src = *req.Source
	}
	info := whatifapi.SourceInfo{Kind: src.Kind}
	f.hint, f.hintSet = hintAPIPrometheus, true
	if src.Kind == whatifapi.SourceRecording {
		f.hint = hintAPIRecording
	}
	var hide string
	uploaded := false
	switch src.Kind {
	case whatifapi.SourceRecording:
		path, release, err := s.recordingPath(src)
		if err != nil {
			return nil, err
		}
		if release != nil {
			uploaded = true
			defer release()
		}
		// The name was a regular file when checked; what is opened is judged
		// again, as opened (openRecording).
		f.recording, f.regularOnly, hide, info.Name = path, true, path, src.Name
		if src.Cycle > 0 {
			f.cycle = strconv.Itoa(src.Cycle)
		}
		if s.onOpen != nil {
			s.onOpen(path)
		}
	case whatifapi.SourcePrometheus:
		if err := s.windowOf(ctx, &f, src); err != nil {
			return nil, err
		}
	default:
		return nil, whatifapi.InputErrorf("source.kind %q, want %s or %s", src.Kind, whatifapi.SourcePrometheus, whatifapi.SourceRecording)
	}
	name, data, err := f.recordingSource(ctx)
	if err != nil {
		return nil, hidePath(apiError(err), hide, info.Name)
	}
	defer func() { _ = data.Close() }()
	data.onPass = s.onPass
	if src.Cycles != nil {
		data.open = learnreplay.CyclesOf(data.open, src.Cycles.Keeps)
	}
	st, err := f.stateOf(ctx, name, data)
	if err != nil {
		return nil, hidePath(apiError(err), hide, info.Name)
	}
	if o := f.captured; o != nil {
		info.Prometheus = redactedURL(f.window.prometheus)
		info.From, info.To = &o.from, &o.to
	}
	info.Cycle, info.TotalCycles, info.FirstAt, info.LastAt = st.Cycle, st.Cycles, st.FirstAt, st.At
	res, err := s.resolved(ctx, &f, st, data, info)
	var serr error
	if err == nil {
		serr = s.experimentalSeries(data, res, req.TTFTModel != "", req.TPOTModel != "")
		res.TPOTModel = req.TPOTModel
	}
	// The series, and the experimental models' after it, are the last
	// passes: a file changed under them is refused, and nothing is kept
	// under the content id its first pass summed.
	if cerr := data.unchanged(); cerr != nil {
		return nil, hidePath(apiError(cerr), hide, info.Name)
	}
	if err == nil && uploaded && s.maxUploadWeight > 0 && res.Bytes > s.maxUploadWeight {
		return nil, whatifapi.TooLargeError{Err: fmt.Errorf("%s: loaded, it takes an estimated %s of memory, more than this server keeps for loaded sources "+
			"(--inputs-cache-size %s): load a shorter recording, or ask whoever runs the planner to raise --inputs-cache-size",
			info.Name, whatifapi.Quantity(int64(res.Bytes)), whatifapi.Quantity(int64(s.maxUploadWeight)))}
	}
	if serr != nil {
		return nil, hidePath(apiError(serr), hide, info.Name)
	}
	return res, err
}

// experimentalSeries reads, where serve prices every backtest with an
// experimental model (--experimental-ttft=v2, --experimental-tpot=v2), the
// cycles that model reads and the engines' flags it takes, in a pass of
// their own over data: the CLI's backtest reads the same (rankMixture,
// tpotDistribution). Neither changes the inputs' content id. ttftModel and
// tpotModel say the request asked for the experimental TTFT or TPOT model
// (whatifapi.InputsRequest.TTFTModel, TPOTModel), which read the same
// series.
func (s *servedSources) experimentalSeries(data *recordingSource, res *whatifapi.Resolved, ttftModel, tpotModel bool) error {
	if s.rankMixture || ttftModel {
		if err := rankMixtureFrom(data.open, res); err != nil {
			return err
		}
		res.Engine.PrefillSlots = prefillSlotsOf(res)
	}
	if s.tpotDistribution || tpotModel {
		if err := decodeSeriesFrom(data.open, res); err != nil {
			return err
		}
		res.Engine.DecodeSlots, res.Engine.SpecTokens = decodeEngineOf(res)
	}
	return nil
}

// recordingPath is the recording a source names: an upload's name, looked
// up and never joined to a path, or a file name in the recordings directory,
// never a path. For an upload, release is non-nil and the caller calls it
// when the load ends: until then the file is kept (uploadStore.load).
func (s *servedSources) recordingPath(src whatifapi.SourceRequest) (path string, release func(), err error) {
	if s.uploads != nil && strings.HasPrefix(src.Name, uploadPrefix) {
		if err := recordingFields(src); err != nil {
			return "", nil, err
		}
		path, release, ok := s.uploads.load(src.Name)
		if !ok {
			return "", nil, whatifapi.InputErrorf("source.name %q: no such upload: the server keeps the last %d uploads until it stops; upload it again",
				src.Name, maxKeptUploads)
		}
		return path, release, nil
	}
	if s.recordings == "" {
		return "", nil, whatifapi.InputErrorf("source.kind recording: the server reads no recordings (--recordings)")
	}
	if err := recordingFields(src); err != nil {
		return "", nil, err
	}
	if !recordingName(src.Name) {
		return "", nil, whatifapi.InputErrorf("source.name %q: want a file name in the server's recordings directory, not a path", src.Name)
	}
	path, ok := s.recordingFile(src.Name)
	if !ok {
		return "", nil, whatifapi.InputErrorf("source.name %q: no such recording in the server's recordings directory", src.Name)
	}
	return path, nil, nil
}

// recordingFields refuses the fields a recording source does not take, and a
// negative cycle.
func recordingFields(src whatifapi.SourceRequest) error {
	switch {
	case src.Namespace != "" || src.From != "" || src.To != "":
		return whatifapi.InputErrorf("source.namespace, from and to are a prometheus source's")
	case src.Cycle < 0:
		return whatifapi.InputErrorf("source.cycle %d, want a cycle from 1, or 0 for the last", src.Cycle)
	}
	return nil
}

// recordingName reports whether name is one a recording source may give: a
// file name, not a path, and not hidden.
func recordingName(name string) bool {
	return name != "" && name == filepath.Base(name) && !strings.ContainsAny(name, `/\:`) && !strings.HasPrefix(name, ".")
}

// recordingFile is name's path in the recordings directory, and whether it
// is a regular file there, a symbolic link followed.
func (s *servedSources) recordingFile(name string) (string, bool) {
	path := filepath.Join(s.recordings, name)
	fi, err := os.Stat(path)
	return path, err == nil && fi.Mode().IsRegular()
}

// maxListedRecordings bounds the recordings GET /v1/sources lists.
const maxListedRecordings = 1000

// Sources is what the server reads, for the page's source picker: whether it
// reads a Prometheus and in which namespaces, and the recordings a recording
// source may name: every name in --recordings that recordingPath accepts (a
// regular file, a link followed, under a name that is no path and not
// hidden), so that nothing listed is refused when picked. The uploads kept
// come first, newest first.
func (s *servedSources) Sources(context.Context) (whatifapi.SourcesResponse, error) {
	out := whatifapi.SourcesResponse{Prometheus: s.window.prometheus != "", Namespaces: slices.Clone(s.allowed), Recordings: []string{}}
	if out.Prometheus {
		out.Namespace = s.window.namespace
		if out.Namespace == "" && len(s.allowed) > 0 {
			out.Namespace = s.allowed[0]
		}
	}
	if s.uploads != nil {
		out.Uploads = s.uploads.list()
		for _, l := range out.Uploads {
			out.Recordings = append(out.Recordings, l.Name)
		}
	}
	if s.recordings == "" {
		return out, nil
	}
	entries, err := os.ReadDir(s.recordings)
	if err != nil {
		// The error would name the server's path: say only that it failed,
		// and log it where there is a log.
		if s.warn != nil {
			fmt.Fprintf(s.warn, "capacity-planner: listing recordings: %v\n", err)
		}
		return out, errors.New("the server could not list its recordings directory; see its log")
	}
	for _, e := range entries {
		// A directory is never a recording; a link is stat'ed below.
		if e.IsDir() || !recordingName(e.Name()) {
			continue
		}
		if _, ok := s.recordingFile(e.Name()); !ok {
			continue
		}
		if len(out.Recordings) == maxListedRecordings {
			out.RecordingsTruncated = true
			break
		}
		out.Recordings = append(out.Recordings, e.Name())
	}
	return out, nil
}

// checkNamespace refuses a namespace a request may not read through the
// server's Prometheus: one that is no DNS-1123 label, one not among the
// allowed, or, with Kubernetes authentication, one whose Pods the user may
// not get.
func (s *servedSources) checkNamespace(ctx context.Context, ns string) error {
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return whatifapi.InputErrorf("source.namespace %q: %s", ns, strings.Join(errs, "; "))
	}
	if !slices.Contains(s.allowed, ns) {
		return whatifapi.ForbiddenError{Err: fmt.Errorf("namespace %s: this server reads %s only (--allowed-namespaces)", ns, strings.Join(s.allowed, ", "))}
	}
	if s.namespaceAllowed != nil {
		return s.namespaceAllowed(ctx, ns)
	}
	return nil
}

// windowOf sets f to read a Prometheus window: the server's Prometheus and
// role map, the request's namespace (else the server's), checked
// (checkNamespace), and window (else the last hour, ending now).
func (s *servedSources) windowOf(ctx context.Context, f *answerFlags, src whatifapi.SourceRequest) error {
	switch {
	case s.window.prometheus == "":
		return whatifapi.InputErrorf("source.kind prometheus: the server reads no Prometheus (--prometheus)")
	case src.Name != "" || src.Cycle != 0:
		return whatifapi.InputErrorf("source.name and cycle are a recording source's")
	case (src.From == "") != (src.To == ""):
		return whatifapi.InputErrorf("source.from and source.to: give both, or neither for the last hour")
	}
	f.window = s.window
	if src.Namespace != "" {
		f.window.namespace = src.Namespace
	}
	if err := s.checkNamespace(ctx, f.window.namespace); err != nil {
		return err
	}
	f.window.from, f.window.to = src.From, src.To
	if src.From == "" {
		now := time.Now
		if s.now != nil {
			now = s.now
		}
		to := now().UTC().Truncate(time.Second)
		f.window.from, f.window.to = to.Add(-defaultWindow).Format(time.RFC3339), to.Format(time.RFC3339)
	}
	f.batchCap = int64(s.window.batchCap)
	return nil
}

// hidePath replaces the server's path to a recording in err by its name: an
// error names no server path.
func hidePath(err error, path, name string) error {
	if path == "" || err == nil || !strings.Contains(err.Error(), path) {
		return err
	}
	msg := errors.New(strings.ReplaceAll(err.Error(), path, name))
	if whatifapi.IsInputError(err) {
		return whatifapi.InputError{Err: msg}
	}
	return msg
}

// resolved reads the fleet from st as the CLI would at the recorded counts,
// and names the inputs by content: the recording's bytes, the cycle, the model
// and the variants.
func (s *servedSources) resolved(ctx context.Context, f *answerFlags, st *learnreplay.State, data *recordingSource, info whatifapi.SourceInfo) (*whatifapi.Resolved, error) {
	variants, err := f.decodeVariants(st)
	if err != nil {
		return nil, apiError(err)
	}
	if len(variants) != 1 {
		names := make([]string, len(variants))
		for i, pv := range variants {
			names[i] = pv.state.Variant
		}
		return nil, whatifapi.InputErrorf("the model has %d decode variants (%s) at cycle %d; a plan prices one P/D fleet: name one with decodeVariant%s",
			len(variants), strings.Join(names, ", "), st.Cycle, noRoleHint(variants))
	}
	ps, err := f.prefillState(st)
	if err != nil {
		return nil, apiError(err)
	}
	q := ttftQuestion{}
	q.policyPercentile(st)
	if q.q == 0 {
		q = ttftQuestion{q: 0.9}
	}
	fleets, err := f.ttftFleets(st, q, variants)
	if err != nil {
		return nil, apiError(err)
	}
	sum := data.sum
	prefill := ""
	if ps != nil {
		prefill = ps.Variant
	}
	content := strings.Join([]string{hex.EncodeToString(sum[:]), st.Namespace, st.ModelID, strconv.Itoa(st.Cycle), prefill,
		variants[0].state.Variant, f.prefillVariant, f.variant}, "\x00")
	series, latency, err := seriesAndLatencyOf(data.open, st, learnreplay.SeriesVariants{Prefill: prefill, Decode: variants[0].state.Variant})
	if err != nil {
		return nil, apiError(err)
	}
	if why := whatifapi.EngineRowsShort(series); why != "" {
		// Every per-replica rate and knee would read low by the ranks the
		// rows leave out (issue llm-d-incubation/llm-d-scaling-manager#331).
		return nil, whatifapi.InputErrorf("%s; the planner reads a replica as its engine rows, so it prices nothing from this recording; %s", why, engineRowsFix)
	}
	pricer := &servedState{st: st, decodeVariant: f.variant, prefillVariant: f.prefillVariant, hint: f.hintSource(),
		imbalanceAcrossCounts: whatif.DefaultPlannerImbalanceAcrossCounts, loadAwareRouter: whatif.DefaultPlannerLoadAwareRouter,
		experimentalTTFT: f.experimentalTTFT, vpServiceLine: f.vpServiceLine, fLoadCurve: f.fLoadCurve}
	res := &whatifapi.Resolved{
		Inputs:  inputsDoc(f, st, &variants[0], ps, fleets[0], info),
		Content: content,
		Pricer:  pricer,
		Series:  series,
		Latency: latency,
	}
	res.Config = runConfigOf(st, variants[0].state.Variant, prefill)
	if f.selfCheckOn() {
		res.Folds, res.PlannerVersion = f.foldFits(variants[0].state.Variant, f.prefillVariant), selfCheckVersion()
		res.Content += "\x00self-check"
	}
	if f.vpPlateau {
		// Another replay, so other inputs: never the id of the default's.
		res.Inputs.PrefillVPPlateau = true
		res.Content += "\x00" + vpPlateauFlag
	}
	if f.hitAtCount {
		// Another replay, so other inputs: never the id of the default's.
		res.Inputs.PrefillHitAtCount = true
		res.Content += "\x00" + hitAtCountFlag
	}
	if f.vpServiceLine {
		// Another replay, so other inputs: never the id of the default's.
		res.Inputs.PrefillVPServiceLine = true
		res.Content += "\x00" + vpServiceLineFlag
	}
	if f.fLoadCurve {
		// Another replay, so other inputs: never the id of the default's.
		res.Inputs.PrefillFLoadCurve = true
		res.Content += "\x00" + fLoadCurveFlag
	}
	if f.experimentalTTFT {
		// Another replay, so other inputs: never the id of the default's.
		res.Inputs.ExperimentalTTFT = true
		res.Content += "\x00" + experimentalTTFTFlag
	}
	f.fitCachedCosts(ctx, ps, res, pricer)
	res.Bytes = heapEstimate(sizeOf(pricer) + sizeOf(series) + sizeOf(latency) + sizeOf(res.Inputs))
	return res, nil
}

// servedState prices fleets from one replayed state. The state is read only,
// so fleets may be priced from it at once.
type servedState struct {
	st                            *learnreplay.State
	decodeVariant, prefillVariant string
	// hint is the words a refusal names a calm cycle in: the source's.
	hint hintSource
	// imbalanceAcrossCounts is --imbalance-across-counts on at-replicas;
	// the server prices at the planner's default.
	imbalanceAcrossCounts bool
	// loadAwareRouter is --load-aware-router on at-replicas; a request's
	// assume.loadAwareRouter turns it on for the fleets it reads.
	loadAwareRouter bool
	// cachedCost is phi as fitted on the source (fitCachedCosts), nil
	// where none was.
	cachedCost *whatifapi.CachedTokenCost
	// experimentalTTFT is --experimental-ttft (the request's
	// experimentalTTFT): every fleet read is priced and flagged as the
	// experimental TTFT model's.
	experimentalTTFT bool
	// vpServiceLine is --prefill-vp-service-line (the request's
	// prefillVPServiceLine): the state was replayed with it, and every
	// fleet read states it (statedOutside).
	vpServiceLine bool
	// fLoadCurve is --prefill-f-load-curve (the request's
	// prefillFLoadCurve): the state was replayed with it, f is priced
	// against the load, and every fleet read states it (statedOutside).
	fLoadCurve bool
}

// seconds is x seconds as a duration, to the nanosecond.
func seconds(x float64) time.Duration { return time.Duration(math.Round(x * float64(time.Second))) }

// Fleet reads the fleet at k: the flags k stands for, checked as the CLI
// checks them, and the fleet slo-to-rate would read with them.
func (s *servedState) Fleet(k whatifapi.Knobs) (whatifapi.Fleet, error) {
	sf, err := s.fleetAt(k)
	if err != nil {
		return nil, err
	}
	return sf, nil
}

// fleetAt is Fleet as the fleet itself, for the report, which reads the
// variant and the prefill inputs it was priced from.
func (s *servedState) fleetAt(k whatifapi.Knobs) (*servedFleet, error) {
	return s.fleetAtImbalance(k, 0)
}

// fleetAtImbalance is fleetAt with f, above zero, at every prefill count
// above one (answerFlags.fixedImbalance): the report's sensitivity to f.
func (s *servedState) fleetAtImbalance(k whatifapi.Knobs, fixedF float64) (*servedFleet, error) {
	f := answerFlags{fixedImbalance: fixedF, question: rateQuestion, cycle: "last", variant: s.decodeVariant, prefillVariant: s.prefillVariant, hint: s.hint, hintSet: true,
		seed: whatif.DefaultResampling().Seed, set: map[string]bool{"ttft": true, "headroom": true}, warn: io.Discard,
		decode: k.Decode, prefill: k.Prefill, headroom: k.Headroom, priceRouterWait: whatif.DefaultPriceRouterWait,
		routerWaitInPrefill: whatif.DefaultRouterWaitInPrefill, psOnsets: whatif.DefaultPSOnsets, chargeDecodeGate: whatif.DefaultChargeDecodeGate, imbalanceAcrossCounts: s.imbalanceAcrossCounts, loadAwareRouter: s.loadAwareRouter,
		experimentalTTFT: s.experimentalTTFT, vpServiceLine: s.vpServiceLine, fLoadCurve: s.fLoadCurve}
	set := func(name string) { f.set[name] = true }
	if k.PromptTokens > 0 {
		f.inputTokens, f.inputTokensRead = k.PromptTokens, k.PromptTokensRead
		set("assume-input-tokens")
	}
	f.prefixHit, f.prefixHitRead = k.PrefixHit, k.PrefixHitRead
	f.prefillComputed = k.PrefillComputedTokens
	if s.cachedCost != nil {
		f.cachedCost, f.cachedPhi = s.cachedCost, s.cachedCost.Phi
	}
	if k.CachedTokenCost != nil {
		f.cachedPhi, f.cachedPhiSet = *k.CachedTokenCost, true
	}
	if k.OutputTokens > 0 {
		f.outputTokens = k.OutputTokens
		set("assume-output-tokens")
	}
	f.stepOutputDispersion = k.OutputDispersion
	a := k.Assume
	if a.VP != nil {
		f.assumeVP = *a.VP
		set("assume-vp")
	}
	if a.A != nil {
		f.assumeA = seconds(*a.A)
		set("assume-a")
	}
	if a.PromptCV != nil {
		f.assumePromptCV = *a.PromptCV
		f.statedPromptMaxRatio = k.PromptMaxRatio
		set("assume-prompt-cv")
	}
	if a.RouterHop != nil {
		f.assumeRouterHop = seconds(*a.RouterHop)
		set("assume-router-hop")
	}
	if a.TPOTRatio != nil {
		f.tpotRatio = *a.TPOTRatio
		set("assume-tpot-ratio")
	}
	switch {
	case a.ITLBFromFloor:
		f.assumeITLB = itlBFloor
		set("assume-itl-b")
	case a.ITLB != nil:
		f.assumeITLB = seconds(*a.ITLB).String()
		set("assume-itl-b")
	}
	if k.ArrivalCV2 != nil {
		f.arrivalCV2 = *k.ArrivalCV2
		set("arrival-cv2")
	}
	if err := f.assumeFlowControl(a); err != nil {
		return nil, apiError(err)
	}
	if a.LoadAwareRouter {
		// --load-aware-router, marked set as the CLI's parse marks it, so the
		// envelope classes it alike (statedOutside).
		f.loadAwareRouter = true
		set("load-aware-router")
	}
	if err := f.checkTTFT(); err != nil {
		return nil, apiError(err)
	}
	variants, err := f.decodeVariants(s.st)
	if err != nil {
		return nil, apiError(err)
	}
	if len(variants) != 1 {
		return nil, fmt.Errorf("%d decode variants priced, want the one the inputs read", len(variants))
	}
	q := ttftQuestion{q: k.TTFTPercentile}
	fleets, err := f.ttftFleets(s.st, q, variants)
	if err != nil {
		return nil, apiError(err)
	}
	sf := &servedFleet{f: &f, st: s.st, pv: variants[0], pf: fleets[0], q: q, tpotQ: k.TPOTPercentile,
		assumed: slices.Clone(fleets[0].in.Assumed)}
	if k.TPOTPercentile != 0 {
		sf.ratio = f.ratioFor(s.st, k.TPOTPercentile)
	}
	return sf, nil
}

// assumeFlowControl sets the flags assume's flow control stands for: the
// CLI's --flow-control, and --router-wait-in-prefill or --price-router-wait,
// each marked set as the CLI's parse marks it, so the envelope classes them
// alike (statedOutside): gateStage, --price-router-wait, puts the figures
// outside; inPrefill (the default) and none, --router-wait-in-prefill on or
// off, state nothing, a figure the switch prices carrying
// FlowControlWaitPriced, which is outside on its own.
func (f *answerFlags) assumeFlowControl(a whatifapi.Assume) error {
	if err := a.CheckFlowControl(); err != nil {
		return usageError{err}
	}
	if a.FlowControl != "" {
		f.flowControl = a.FlowControl
		f.set["flow-control"] = true
	}
	switch a.RouterWait {
	case whatifapi.RouterWaitNone:
		f.routerWaitInPrefill, f.priceRouterWait = false, false
	case whatifapi.RouterWaitInPrefill:
		f.routerWaitInPrefill, f.priceRouterWait = true, false
		f.set["router-wait-in-prefill"] = true
	case whatifapi.RouterWaitGateStage:
		f.priceRouterWait, f.routerWaitInPrefill = true, false
		f.set["price-router-wait"] = true
	}
	return nil
}

// servedFleet is one fleet, priced figure by figure as the CLI prices it. It
// is one request's, never shared.
type servedFleet struct {
	f     *answerFlags
	st    *learnreplay.State
	pv    pricedVariant
	pf    *pricedFleet
	q     ttftQuestion
	tpotQ float64
	ratio resolvedRatio
	// model is the experimental model's R_q priced in place of the fleet's
	// own (WithTPOTRatio); nil prices the fleet's.
	model *whatifapi.ModelRatio
	// assumed is the fleet's assumed row as read, before any figure added to
	// it.
	assumed []string
}

// WithTPOTRatio is sf with its TPOT percentile priced at the experimental
// model's R_q, as an assumed R_q is priced, flagged r.Flag where the
// assumed one is flagged AssumedTPOTRatio: the percentile curve, its point
// and decode's limit at the target. The host-bound refusal, a coarse
// bucket's reading, does not apply: the model's spread is fitted on every
// bucket.
func (sf *servedFleet) WithTPOTRatio(r whatifapi.ModelRatio) whatifapi.Fleet {
	c := *sf
	c.ratio = resolvedRatio{ratio: whatif.TPOTRatio{Value: r.Value}, flags: append([]whatif.Flag{r.Flag}, r.More...)}
	c.model = &r
	c.assumed = append(slices.Clone(sf.assumed), r.Assumed)
	return &c
}

// PrefillKnee is replicas-to-slo's prefill knee.
func (sf *servedFleet) PrefillKnee() whatif.Result { return sf.pf.knee() }

// DecodeKnee is replicas-to-slo's decode knee.
func (sf *servedFleet) DecodeKnee() whatif.Result {
	return sf.pv.qualify(sf.pv.band(whatif.KneeRate(sf.pv.decode), whatif.KneeRate))
}

// Onsets is the fleet's flow-control onsets; nil with flow control off, or a
// fleet refused before pricing.
func (sf *servedFleet) Onsets() whatif.Result {
	if sf.pf.refused != nil || sf.pf.fleet.FlowControl.State != whatif.FlowControlOn {
		return nil
	}
	return whatif.FlowControlOnsets(sf.pf.fleet)
}

// TTFTAt is the TTFT at rate with its span, as replicas-to-slo --rate prices
// it, less the assumed words it adds.
func (sf *servedFleet) TTFTAt(rate float64) whatif.Result {
	price := func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, rate, sf.q.q) }
	return withSpan(sf.pf.priced(price), sf.pf.corners(price))
}

// TTFTPoint is replicas-to-slo --rate's TTFT, and the words it adds to the
// assumed row.
func (sf *servedFleet) TTFTPoint(rate float64) (whatif.Result, []string) {
	n := len(sf.pf.in.Assumed)
	o, _ := sf.pf.atOperatingPoint(sf.q, whatif.ReplicasQuery{}, &rate, 0, nil)
	return o.ttft, slices.Clone(sf.pf.in.Assumed[n:])
}

// TPOTAt is replicas-to-slo --rate's mean TPOT and percentile.
func (sf *servedFleet) TPOTAt(rate float64) (mean, percentile whatif.Result) {
	query := whatif.ReplicasQuery{Rate: rate}
	if sf.tpotQ != 0 {
		query.PercentileTarget = percentileTarget(sf.tpotQ, sf.ratio)
	}
	a, _ := replicasAt(sf.pv, nil, nil, query, &rate, 0, sf.ratio)
	return a.slo.MeanTPOT, a.slo.PercentileTPOT
}

// decodeRate is slo-to-rate's decode rate at headroom. With no TPOT target it
// is nil, or decode's knee's refusal where the knee is refused
// (kneeRefusal): the fleet's limit is then that refusal too.
func (sf *servedFleet) decodeRate(tpotSeconds, headroom float64) whatif.Result {
	if !(tpotSeconds > 0) {
		return kneeRefusal(sf.pv)
	}
	target := whatif.TPOTTarget{Seconds: tpotSeconds, Percentile: sf.tpotQ, Ratio: sf.ratio.ratio}
	return decodeRate(sf.pv, rateQuery(target, headroom, sf.ratio), sf.ratio)
}

// Limits is slo-to-rate's decode, prefill and fleet rates, and the fleet's
// at headroom 1 from the same prefill rate.
func (sf *servedFleet) Limits(ttftSeconds, tpotSeconds, headroom float64) whatifapi.LimitSet {
	n := len(sf.pf.in.Assumed)
	t := ttftQuestion{q: sf.q.q, seconds: ttftSeconds}
	a := rateAnswer{pv: sf.pv, fleet: sf.pf, result: sf.decodeRate(tpotSeconds, headroom)}
	corners := a.priceTTFTRate(t)
	return whatifapi.LimitSet{Decode: a.result, Prefill: a.ttft, Fleet: a.fleetAt(t, headroom, a.result, corners),
		WithoutHeadroom: a.fleetAt(t, 1, sf.decodeRate(tpotSeconds, 1), corners), Notes: slices.Clone(sf.pf.in.Assumed[n:])}
}

// PlainLimits is Limits' rates at headroom with no span: the prefill role's
// rate is RateForTTFT's alone, not swept over V_P and f.
func (sf *servedFleet) PlainLimits(ttftSeconds, tpotSeconds, headroom float64) (decode, prefill, fleet whatif.Result) {
	t := ttftQuestion{q: sf.q.q, seconds: ttftSeconds}
	decode = sf.decodeRate(tpotSeconds, headroom)
	prefill = sf.pf.priced(func(fl whatif.TTFTFleet) whatif.Result {
		return whatif.RateForTTFT(fl, whatif.TTFTTarget{Seconds: t.seconds, Percentile: t.q})
	})
	return decode, prefill, fleetRate(t, headroom, decode, prefill)
}

// TPOTRatio is R_q as the percentile asked resolved it.
func (sf *servedFleet) TPOTRatio() whatifapi.Ratio {
	if sf.tpotQ == 0 {
		return whatifapi.Ratio{}
	}
	if u := sf.ratio.refused; u != nil {
		return whatifapi.Ratio{Refused: &whatifapi.Refusal{Reason: string(u.Reason), Detail: u.Detail, Flags: whatifapi.FlagNames(u.Flags)}}
	}
	r := sf.ratio.ratio.Value
	out := whatifapi.Ratio{Value: &r, Source: sourceLearned, Flags: whatifapi.FlagNames(sf.ratio.flags)}
	switch {
	case sf.model != nil:
		out.Source = sf.model.Source
	case sf.ratio.ratio.Assumed:
		out.Source = shapeAssumed
		out.Flags = append(out.Flags, string(whatif.AssumedTPOTRatio))
	case sf.f.engineTPOT:
		out.Source = "engine histogram"
	}
	return out
}

// Assumed is the fleet's assumed inputs as read, in the CLI's words.
func (sf *servedFleet) Assumed() []string { return sf.assumed }

// Class is the class of the fleet's figures, as the CLI judges them.
func (sf *servedFleet) Class() whatif.FigureClass { return sf.pf.class() }
