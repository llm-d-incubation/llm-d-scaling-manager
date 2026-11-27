package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// serveFlags is serve's flags: where to listen and how to authenticate, and
// the sources it reads, as capture's flags give a Prometheus window.
type serveFlags struct {
	window           captureFlags
	recordings       string
	listen           string
	auth             string
	tlsCert, tlsKey  string
	shutdownDeadline time.Duration
	// planBudget is --plan-budget, how long one plan may be priced; 0 is
	// whatifapi.DefaultPlanBudget.
	planBudget time.Duration
	// maxInputs is --max-inputs, how many sources are resolved at once, and
	// inputsCache --inputs-cache-size, the bound in bytes of the inputs kept;
	// 0 and "" are whatifapi's defaults.
	maxInputs   int
	inputsCache string
	// allowedNamespaces is --allowed-namespaces, comma-separated, and
	// audiences --token-audiences.
	allowedNamespaces, audiences string
	// allInContainer is --listen-all-in-container: the one way to bind every
	// interface without authentication (whatifapi.CheckContainerListen).
	allInContainer bool
	// inPod is whether serve runs in a Kubernetes Pod (inKubernetesPod),
	// where --listen-all-in-container is refused.
	inPod bool
	// allowUpload is --allow-upload, and uploadMax --upload-max-size.
	allowUpload bool
	// noSelfCheck is --no-self-check: inputs do not self-check unless asked.
	noSelfCheck bool
	// experimentalTTFT is --experimental-ttft: every request is priced with
	// the experimental TTFT model, so a page user need not ask for it;
	// rankMixtureTTFT is its =v2, every backtest priced with the
	// rank-mixture TTFT model (experimentalTTFTValue, as the CLI's), and
	// the main view of every source; rankMixtureNLLine is =v2nl-line, the
	// same with the variant whatif.QueueRankMixtureNLLine.
	experimentalTTFT, rankMixtureTTFT, rankMixtureNLLine bool
	// rankMixtureStratified is --rank-mixture-stratified: the server's
	// default for a request that does not say, nil the planner's
	// (whatifapi.Config.NoRankMixtureStratified).
	rankMixtureStratified *bool
	// replay is the opt-in replay switches serve turns on for every source
	// (serve_replay.go).
	replay replayDefaults
	// rankMixtureFit is the variant's modes and the shortest fit window
	// (rank_mixture_fit.go): the server's defaults for a backtest or a
	// source that does not set them (whatifapi.Config
	// RankMixtureKVHold, RankMixtureRemainderSpread,
	// RankMixtureMinFitWindow).
	rankMixtureFit rankMixtureFit
	// tpotFit is --tpot-dist-min-fit-steps (rank_mixture_fit.go): the
	// server's default (whatifapi.Config.TPOTDistMinFitSteps).
	tpotFit tpotDistFit
	// experimentalTPOT is --experimental-tpot (=v2): every backtest, and
	// the main view of every source, priced with the per-request TPOT
	// distribution.
	experimentalTPOT string
	uploadMax        string
	// uploads is the store handler made with --allow-upload, for the caller
	// to Close when the server stops; nil without.
	uploads *uploadStore
}

// runServe serves the capacity planner's API (internal/whatifapi) until ctx
// ends: POST /v1/inputs, POST /v1/plan and POST /v1/size, priced on the
// CLI's own code paths. A Prometheus window is read from --prometheus with capture's
// flags, the window and namespace coming with each request; a recording is
// read from --recordings by file name, or, with --allow-upload, uploaded
// with POST /v1/recordings and named by the server. It binds a loopback address unless
// --auth=kubernetes, which needs TLS, or --listen-all-in-container, which
// binds every interface of a container with no authentication.
func runServe(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newFlags("serve", out)
	var f serveFlags
	f.window.registerWindow(fs)
	fs.Lookup("prometheus").Usage = "the Prometheus base URL that inputs with a prometheus source read; empty serves recordings only"
	for _, name := range []string{"from", "to"} {
		fs.Lookup(name).Usage = "not for serve: each request names its own window"
	}
	fs.StringVar(&f.window.namespace, "namespace", "", "the namespace a prometheus source reads when the request names none")
	fs.IntVar(&f.window.batchCap, "batch-cap", 0, "the engine's batch cap a window is captured with, as capture --batch-cap")
	fs.StringVar(&f.recordings, "recordings", "", "a directory of learner recordings a recording source names by file name")
	fs.StringVar(&f.listen, "listen", "127.0.0.1:8080", "the address to serve on; a non-loopback one needs --auth=kubernetes")
	fs.StringVar(&f.auth, "auth", whatifapi.AuthNone, "none (loopback only) or kubernetes (a bearer token checked with TokenReview and SubjectAccessReview)")
	fs.StringVar(&f.tlsCert, "tls-cert-file", "", "the server's certificate, PEM; required with --auth=kubernetes")
	fs.StringVar(&f.tlsKey, "tls-key-file", "", "the server's private key, PEM; required with --auth=kubernetes")
	fs.DurationVar(&f.shutdownDeadline, "shutdown-timeout", 10*time.Second, "how long requests in flight may run once the server is told to stop")
	fs.DurationVar(&f.planBudget, "plan-budget", whatifapi.DefaultPlanBudget, "how long one plan may be priced; past it the plan is refused with PlanBudgetExceeded")
	fs.IntVar(&f.maxInputs, "max-inputs", whatifapi.DefaultMaxInputs, "how many sources are resolved at once; each holds its replay's memory while it runs, and past them a request is 503 with Retry-After")
	fs.StringVar(&f.inputsCache, "inputs-cache-size", whatifapi.Quantity(whatifapi.DefaultInputsBytes), "the most memory the resolved sources kept may take in all, as estimated, a quantity (256Mi); the least used is dropped first, and the newest is kept whatever it weighs")
	fs.StringVar(&f.allowedNamespaces, "allowed-namespaces", "", "the namespaces a prometheus source may read, comma-separated; default --namespace alone")
	fs.BoolVar(&f.allInContainer, "listen-all-in-container", false, "in a container whose port is published to the host's loopback only (-p 127.0.0.1:8080:8080): allow --listen 0.0.0.0:PORT with --auth=none; never where the address is reachable from a network. Refused when KUBERNETES_SERVICE_HOST is set, a best-effort tripwire, not enforcement")
	fs.StringVar(&f.audiences, "token-audiences", "", "with --auth=kubernetes, the audiences a TokenReview checks a token for, comma-separated; default the API server's")
	fs.Var(experimentalTTFTValue{switches: &f.experimentalTTFT, v2: &f.rankMixtureTTFT, nlLine: &f.rankMixtureNLLine}, experimentalTTFTFlag, "price every request with the experimental TTFT model, as if each asked for experimentalTTFT: for a planner served to people who should see it without asking; =v2 instead prices every backtest with the rank-mixture TTFT model, as if each asked for ttftModel "+string(whatif.QueueRankMixture)+", and the main view of every source (plan, size, report). "+experimentalTTFTV2Usage+"; "+experimentalTTFTNLLineUsage+", as if each asked for ttftModel "+string(whatif.QueueRankMixtureNLLine))
	fs.Var(optionalBool{&f.rankMixtureStratified, whatif.DefaultRankMixtureStratified}, rankMixtureStratifiedFlag, rankMixtureStratifiedUsage+"; a request's rankMixtureStratified overrides it")
	f.replay.register(fs)
	f.rankMixtureFit.registerModes(fs)
	f.rankMixtureFit.registerWindow(fs)
	f.tpotFit.register(fs)
	fs.StringVar(&f.experimentalTPOT, experimentalTPOTFlag, "", "price every backtest, and the main view's TPOT p90 and p95 (plan, size, report), with the experimental per-request TPOT distribution, as if each asked for tpotModel "+whatif.TPOTRenewalLognormal+": "+experimentalTPOTUsage)
	fs.BoolVar(&f.noSelfCheck, "no-self-check", false, "do not check the planner on each source beside its inputs, unless a request asks (selfCheck true)")
	fs.BoolVar(&f.allowUpload, "allow-upload", false, "take recordings uploaded from the page (POST /v1/recordings): kept as sent, in a private temporary directory (TMPDIR; TMP on Windows), the last 4, until the server stops. Off by default: whoever may open the planner may then upload")
	fs.StringVar(&f.uploadMax, "upload-max-size", whatifapi.Quantity(whatifapi.DefaultUploadBytes), "with --allow-upload, the largest upload, a quantity (256Mi); it may gunzip to 16 times that")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	f.inPod = inKubernetesPod()
	handler, err := f.handler(fs, warn)
	if err != nil {
		return err
	}
	defer func() { _ = f.uploads.Close() }()
	// The analyzers a replay runs log through controller-runtime, which the
	// CLI never shows; a server lives past the 30 s after which an unset
	// logger says so on stderr, with a stack.
	ctrllog.SetLogger(logr.Discard())
	srv := newHTTPServer(f.listen, handler)
	// The key pair is read before the port is taken, so a bad one leaves no
	// listener behind.
	if f.auth == whatifapi.AuthKubernetes {
		cert, err := tls.LoadX509KeyPair(f.tlsCert, f.tlsKey)
		if err != nil {
			return usagef("--tls-cert-file, --tls-key-file: %v", err)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		return err
	}
	if srv.TLSConfig != nil {
		ln = tls.NewListener(ln, srv.TLSConfig)
	}
	fmt.Fprintf(warn, "capacity-planner: serving the capacity planner's API on %s (auth %s)\n", ln.Addr(), f.auth)
	if f.uploads != nil {
		fmt.Fprintf(warn, "capacity-planner: --allow-upload: whoever may open the planner may upload a recording; "+
			"uploads are kept in %s until the server stops\n", f.uploads.dir)
	}
	// Warned only once the port is taken, so a start refused or a bind that
	// fails prints no warning.
	if f.allInContainer {
		fmt.Fprintf(warn, "capacity-planner: warning: --listen-all-in-container: serving %s with no authentication. "+
			"The Host check is not access control: any client that reaches the port can send Host: localhost. "+
			"Publish the port on the host's loopback only (-p 127.0.0.1:PORT:PORT), never beyond it\n", ln.Addr())
	}
	if listening != nil {
		listening(ln.Addr())
	}
	return serveUntil(ctx, srv, ln, f.shutdownDeadline)
}

// listening, when set, is called with the address serve bound, once it
// serves: tests read the port from it rather than from stderr.
var listening func(net.Addr)

// inKubernetesPod reports whether this process runs in a Kubernetes Pod, by
// the KUBERNETES_SERVICE_HOST the kubelet sets in every container.
func inKubernetesPod() bool {
	return os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}

// shutdownGrace is how long requests in flight have to return once their
// contexts end at the shutdown deadline: a plan stops at its next figure.
const shutdownGrace = 2 * time.Second

// serveUntil serves srv on ln until ctx ends, then shuts it down: requests in
// flight may run until deadline, when their contexts end (srv's BaseContext)
// so that a plan stops between figures; after shutdownGrace what is left is
// closed.
func serveUntil(ctx context.Context, srv *http.Server, ln net.Listener, deadline time.Duration) error {
	base, stop := context.WithCancel(context.Background())
	defer stop()
	srv.BaseContext = func(net.Listener) context.Context { return base }
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	atDeadline := time.AfterFunc(deadline, stop)
	defer atDeadline.Stop()
	sctx, cancel := context.WithTimeout(context.Background(), deadline+shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		<-done
		return fmt.Errorf("shutting down after %s: %w", deadline, err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// handler checks the flags and builds the API's handler, wrapped in
// Kubernetes authentication with --auth=kubernetes; the window's --from and
// --to are refused among the flags set on fs. The client is built from the
// kubeconfig's default loading rules (KUBECONFIG, else ~/.kube/config, else
// in-cluster).
func (f *serveFlags) handler(fs *flag.FlagSet, warn io.Writer) (http.Handler, error) {
	var err error
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "from" || fl.Name == "to" {
			err = usagef("--%s: serve reads the window each request names", fl.Name)
		}
		if fl.Name == "upload-max-size" && !f.allowUpload {
			err = usagef("--upload-max-size needs --allow-upload")
		}
	})
	if err != nil {
		return nil, err
	}
	if f.allInContainer {
		if err := whatifapi.CheckContainerListen(f.listen, f.auth, f.inPod); err != nil {
			return nil, usageError{err}
		}
	} else if err := whatifapi.CheckListen(f.listen, f.auth); err != nil {
		return nil, usageError{err}
	}
	if f.auth == whatifapi.AuthKubernetes && (f.tlsCert == "" || f.tlsKey == "") {
		return nil, usagef("--auth=%s needs --tls-cert-file and --tls-key-file: a bearer token is not sent in the clear", whatifapi.AuthKubernetes)
	}
	if f.planBudget < 0 || f.planBudget > maxPlanBudget {
		return nil, usagef("--plan-budget %s: want 0 (the default, %s) up to %s",
			f.planBudget, whatifapi.DefaultPlanBudget, maxPlanBudget)
	}
	if f.maxInputs < 0 {
		return nil, usagef("--max-inputs %d: want 1 or more, or 0 for the default, %d", f.maxInputs, whatifapi.DefaultMaxInputs)
	}
	cacheBytes := 0
	if f.inputsCache != "" {
		var err error
		if cacheBytes, err = bytesFlag("inputs-cache-size", f.inputsCache); err != nil {
			return nil, err
		}
	}
	var uploadMax int
	if f.allowUpload {
		v := f.uploadMax
		if v == "" {
			v = whatifapi.Quantity(whatifapi.DefaultUploadBytes)
		}
		if uploadMax, err = bytesFlag("upload-max-size", v); err != nil {
			return nil, err
		}
	}
	if f.recordings == "" && f.window.prometheus == "" && !f.allowUpload {
		return nil, usagef("serve needs --prometheus, --recordings or --allow-upload: a source to resolve inputs from")
	}
	if f.recordings != "" {
		if fi, err := os.Stat(f.recordings); err != nil || !fi.IsDir() {
			return nil, usagef("--recordings %s: not a directory", f.recordings)
		}
	}
	allowed, err := f.namespaces()
	if err != nil {
		return nil, err
	}
	tpotModel, err := experimentalTPOTOf(f.experimentalTPOT)
	if err != nil {
		return nil, err
	}
	if err := f.rankMixtureFit.check(f.rankMixtureTTFT, f.rankMixtureNLLine); err != nil {
		return nil, err
	}
	if err := f.tpotFit.check(tpotModel != ""); err != nil {
		return nil, err
	}
	warnKVHoldAlone(warn, f.rankMixtureFit.kvHold, f.rankMixtureFit.remainderSpread)
	src := &servedSources{window: f.window, recordings: f.recordings, warn: warn, allowed: allowed, experimentalTTFT: f.experimentalTTFT,
		rankMixture: f.rankMixtureTTFT, tpotDistribution: tpotModel != "", replay: f.replay}
	cfg := whatifapi.Config{Resolver: src, WhatifSchema: "schema-v" + strconv.Itoa(schemaVersion), PlannerVersion: plannerVersion(), SelfCheck: defaultSelfCheck && !f.noSelfCheck, PlanBudget: f.planBudget,
		MaxInputs: f.maxInputs, MaxInputsBytes: cacheBytes, NoRankMixtureStratified: !stratifiedOr(f.rankMixtureStratified),
		RankMixtureKVHold: f.rankMixtureFit.kvHold, RankMixtureRemainderSpread: f.rankMixtureFit.remainderSpread,
		RankMixtureRemainderServer: f.rankMixtureFit.remainderServer,
		RankMixtureMinFitWindow:    f.rankMixtureFit.window(), TPOTDistMinFitSteps: f.tpotFit.steps()}
	if f.rankMixtureTTFT {
		cfg.TTFTModel = rankMixtureModel(f.rankMixtureNLLine)
	}
	cfg.TPOTModel = tpotModel
	if f.allowUpload {
		// Built last, so that no check refusing the flags leaves its
		// directory behind; the client below is the one step after it.
		up, err := newUploadStore(int64(uploadMax), warn)
		if err != nil {
			return nil, usagef("--allow-upload: no private temporary directory (%v): set TMPDIR (TMP on Windows) to a writable directory, "+
				"or in a Pod with a read-only root mount an emptyDir at /tmp", err)
		}
		f.uploads, src.uploads = up, up
		src.maxUploadWeight = cacheBytes
		if src.maxUploadWeight == 0 {
			src.maxUploadWeight = whatifapi.DefaultInputsBytes
		}
		cfg.Uploader, cfg.UploadMaxBytes = up, int64(uploadMax)
	}
	h := whatifapi.New(cfg).Handler()
	if f.auth != whatifapi.AuthKubernetes {
		return whatifapi.LoopbackOnly(h), nil
	}
	a, err := f.authenticator(warn)
	if err != nil {
		_ = f.uploads.Close()
		f.uploads = nil
		return nil, err
	}
	src.namespaceAllowed = a.NamespaceAllowed
	return a.Wrap(h), nil
}

// authenticator is --auth=kubernetes's authenticator, its client built from
// the kubeconfig's default loading rules.
func (f *serveFlags) authenticator(warn io.Writer) (*whatifapi.Authenticator, error) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("--auth=%s: no Kubernetes client: %w", whatifapi.AuthKubernetes, err)
	}
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("--auth=%s: %w", whatifapi.AuthKubernetes, err)
	}
	return whatifapi.NewAuthenticator(c, splitList(f.audiences), 0, func(format string, args ...any) {
		fmt.Fprintf(warn, format+"\n", args...)
	}), nil
}

// newHTTPServer is the server on addr, with its timeouts: a client may not
// hold a connection open by sending slowly, and a response may take as long
// as a cold plan.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: readTimeout,
		WriteTimeout: writeTimeout, IdleTimeout: idleTimeout}
}

// The server's timeouts. A write may run as long as a cold plan, or a capture
// of a Prometheus window, takes. A route that computes moves its write deadline
// to whatifapi.LongRequestDeadline once its body is read, and an upload to its
// own; these hold for the rest.
const (
	readTimeout  = 30 * time.Second
	writeTimeout = 10 * time.Minute
	idleTimeout  = 2 * time.Minute
)

// maxPlanBudget is the largest --plan-budget. A plan stopped at its budget,
// which may run on for one figure, is refused on a connection still open: a
// route that plans has whatifapi.LongRequestDeadline, 30 minutes, and a
// backtest may first load a recording within that same time.
const maxPlanBudget = 9 * time.Minute

// namespaces is the namespaces a prometheus source may read: --allowed-namespaces,
// else --namespace alone, each a DNS-1123 label. With --prometheus, one is
// needed: the server reads Prometheus with its own credentials, so a request
// must not pick any namespace they reach.
func (f *serveFlags) namespaces() ([]string, error) {
	if f.window.prometheus == "" {
		return nil, nil
	}
	allowed := splitList(f.allowedNamespaces)
	if len(allowed) == 0 && f.window.namespace != "" {
		allowed = []string{f.window.namespace}
	}
	if len(allowed) == 0 {
		return nil, usagef("--prometheus needs --namespace or --allowed-namespaces: the namespaces a request may read with the server's credentials")
	}
	for _, ns := range allowed {
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return nil, usagef("namespace %q: %s", ns, strings.Join(errs, "; "))
		}
	}
	if f.window.namespace != "" && !slices.Contains(allowed, f.window.namespace) {
		return nil, usagef("--namespace %s is not among --allowed-namespaces", f.window.namespace)
	}
	return allowed, nil
}

// splitList is a comma-separated list, blanks dropped.
func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// bytesFlag is the quantity --name gives (256Mi, 1G, 1048576) in bytes, at
// least 1.
func bytesFlag(name, v string) (int, error) {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return 0, usagef("--%s %q: want a quantity of bytes such as 256Mi: %v", name, v, err)
	}
	n, ok := q.AsInt64()
	if !ok || n < 1 || n > 1<<47 {
		return 0, usagef("--%s %s: want a whole number of bytes from 1 to 128Ti", name, v)
	}
	return int(n), nil
}
