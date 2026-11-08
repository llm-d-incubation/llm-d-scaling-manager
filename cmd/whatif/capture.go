package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource"
)

// schemeHTTPS is the scheme the bearer token and TLS are for.
const schemeHTTPS = "https"

// defaultTimeout is how long one HTTP request to Prometheus may take: a
// range query over a long window at a short step is slow, and Prometheus'
// own query timeout defaults to 2 minutes, so a little more than that.
const defaultTimeout = 150 * time.Second

// repeated is a flag that may be given more than once.
type repeated []string

// String implements flag.Value: the values given, joined by commas.
func (r *repeated) String() string { return strings.Join(*r, ",") }

// Set implements flag.Value: one more value.
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// captureFlags is capture's flags, as given.
type captureFlags struct {
	prometheus, tokenFile, caFile string
	insecure                      bool
	timeout                       time.Duration
	namespace                     string
	ksm                           bool
	roleOf                        repeated
	from, to                      string
	step                          time.Duration
	policy                        string
	engine, engineArgs            string
	batchCap, gpus                int
	accelerator                   string
	force                         bool
	out                           string
}

func runCapture(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newFlags("capture", out)
	var f captureFlags
	f.registerWindow(fs)
	fs.StringVar(&f.namespace, "namespace", "", "the namespace whose models to capture (required)")
	fs.IntVar(&f.batchCap, "batch-cap", 0, "the engine's batch cap (--max-num-seqs); 0 leaves the engine's default")
	fs.StringVar(&f.out, "o", "", "where to write the capture, gzipped JSON lines (required)")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	opts, err := f.options("capture", warn)
	if err != nil {
		return redactedError(err, f.prometheus)
	}
	// Written beside -o and renamed over it once whole: a failed or
	// cancelled capture leaves the file that was there.
	var sum *whatifsource.Summary
	err = writeFileAtomic(f.out, func(w io.Writer) error {
		var err error
		sum, err = whatifsource.Capture(ctx, opts, w)
		return err
	})
	if err != nil {
		return redactedError(captureError(err), f.prometheus)
	}
	fmt.Fprintf(out, "captured %d steps of %s from %s to %s every %s: %d cycles, %d rows, %d variants, %d range queries, %d samples\n",
		sum.Steps, opts.Namespace, opts.From.Format(time.RFC3339), opts.To.Format(time.RFC3339), opts.Step,
		sum.Cycles, sum.Rows, len(sum.Variants), sum.RangeQueries, sum.Samples)
	for _, v := range sum.Variants {
		role := v.Role
		if role == "" {
			role = "both"
		}
		fmt.Fprintf(out, "  %s/%s  %s  %d GPU %s\n", v.ModelID, v.Name, role, v.GpuCount, v.Accelerator)
	}
	for _, flag := range sum.Flags {
		fmt.Fprintf(out, "  flag: %s\n", flag)
	}
	fmt.Fprintf(out, "wrote %s\n", f.out)
	return nil
}

// captureError is a failed capture's error with capture's exit code: 2 for
// the caller's input (whatifsource.IsInputError, or a usage error such as
// an output that cannot be created), 1 for anything else, a failed query
// included.
func captureError(err error) error {
	var u usageError
	switch {
	case whatifsource.IsInputError(err):
		return usageError{fmt.Errorf("capture: %w", err)}
	case errors.As(err, &u):
		return usageError{fmt.Errorf("capture: %w", u.error)}
	}
	return fmt.Errorf("capture: %w", err)
}

// windowFlags is the flags registerWindow registers but --prometheus, by
// name, read from the flag set it fills so that the two cannot disagree:
// the answer commands take them only with --prometheus.
func windowFlags() []string {
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	var f captureFlags
	f.registerWindow(fs)
	var names []string
	fs.VisitAll(func(fl *flag.Flag) {
		if fl.Name != "prometheus" {
			names = append(names, fl.Name)
		}
	})
	return names
}

// registerWindow registers the flags that say which window to read from
// Prometheus, and how: every capture flag but --namespace, --batch-cap and
// -o, which the answer commands register as flags of their own.
func (f *captureFlags) registerWindow(fs *flag.FlagSet) {
	fs.StringVar(&f.prometheus, "prometheus", "", "the Prometheus base URL (required)")
	fs.StringVar(&f.tokenFile, "bearer-token-file", "", "a file holding a bearer token, sent to an https Prometheus only, and to its host only")
	fs.StringVar(&f.caFile, "ca-file", "", "a PEM file of the CAs that sign an https Prometheus' certificate, trusted in place of the system's")
	fs.BoolVar(&f.insecure, "insecure-skip-verify", false, "do not verify Prometheus' certificate (warned about on stderr)")
	fs.DurationVar(&f.timeout, "timeout", defaultTimeout, "how long one HTTP request to Prometheus may take")
	fs.BoolVar(&f.ksm, "kube-state-metrics", false, "read Pods, owners, readiness, GPUs and nodes from kube-state-metrics in the same Prometheus")
	fs.Var(&f.roleOf, "role-of", "ROLE=SELECTOR: the Pods the label selector picks serve ROLE (prefill, decode, both), as one variant; repeat per variant")
	fs.StringVar(&f.from, "from", "", "the window's start: RFC 3339 or Unix seconds, to the millisecond (required)")
	fs.StringVar(&f.to, "to", "", "the window's end: RFC 3339 or Unix seconds, to the millisecond (required)")
	fs.DurationVar(&f.step, "step", 15*time.Second, "the interval between cycles, taken as the run's optimize interval")
	fs.StringVar(&f.policy, "policy", "", "a scaling-policy entry (YAML), applied as the default entry; the defaults when absent")
	fs.StringVar(&f.engine, "engine", "vllm", "the inference engine the variants run: vllm or sglang")
	fs.StringVar(&f.engineArgs, "engine-args", "", "the engine's arguments, space-separated, for its parameters (kube-state-metrics has none)")
	fs.IntVar(&f.gpus, "gpus-per-replica", 1, "GPUs per replica, where kube-state-metrics gives none")
	fs.StringVar(&f.accelerator, "accelerator", "", "the accelerator, where kube-state-metrics gives none")
	fs.BoolVar(&f.force, "force", false, fmt.Sprintf("capture past %d steps times queries, or %d samples held in memory", whatifsource.MaxStepQueries, whatifsource.MaxSamples))
}

// options checks the flags and turns them into whatifsource.Options for
// the command cmd, writing what it warns about to warn. A capture also
// needs -o, which only capture has.
func (f captureFlags) options(cmd string, warn io.Writer) (whatifsource.Options, error) {
	var opts whatifsource.Options
	switch {
	case f.prometheus == "":
		return opts, usagef("%s needs --prometheus", cmd)
	case f.namespace == "":
		return opts, usagef("%s needs --namespace", cmd)
	case f.from == "" || f.to == "":
		return opts, usagef("%s needs --from and --to", cmd)
	case cmd == "capture" && f.out == "":
		return opts, usagef("capture needs -o")
	case f.step <= 0:
		return opts, usagef("--step %v is not positive", f.step)
	case f.timeout <= 0:
		return opts, usagef("--timeout %v is not positive", f.timeout)
	case !f.ksm && len(f.roleOf) == 0:
		return opts, usageError{whatifsource.ErrNoRoleMap}
	}
	from, err := parseTime(f.from)
	if err != nil {
		return opts, usagef("--from: %v", err)
	}
	to, err := parseTime(f.to)
	if err != nil {
		return opts, usagef("--to: %v", err)
	}
	if !to.After(from) {
		return opts, usagef("--to (%s) is not after --from (%s)", to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	for _, r := range f.roleOf {
		parsed, err := whatifsource.ParseRoleOf(r)
		if err != nil {
			return opts, usageError{err}
		}
		opts.RoleOf = append(opts.RoleOf, parsed)
	}
	if err := whatifsource.CheckRoleOf(opts.RoleOf); err != nil {
		return opts, usageError{err}
	}
	batchFlag := "--max-num-seqs"
	switch f.engine {
	case "vllm":
		opts.Engine = inferenceengine.EngineVLLM
	case "sglang":
		opts.Engine = inferenceengine.EngineSGLang
		batchFlag = "--max-running-requests"
	default:
		return opts, usagef("--engine %q is not vllm or sglang", f.engine)
	}
	if f.batchCap < 0 || f.gpus < 1 {
		return opts, usagef("--batch-cap must not be negative and --gpus-per-replica must be at least 1")
	}
	opts.EngineArgs = strings.Fields(f.engineArgs)
	if f.batchCap > 0 && hasFlag(opts.EngineArgs, batchFlag) {
		fmt.Fprintf(warn, "whatif: warning: --batch-cap %d and %s in --engine-args both set the batch cap, "+
			"and may disagree; give one of them\n", f.batchCap, batchFlag)
	}
	if f.policy != "" {
		b, err := os.ReadFile(f.policy)
		if err != nil {
			return opts, usageError{err}
		}
		p, err := config.DecodeScalingPolicy(string(b))
		if err != nil {
			return opts, usagef("--policy %s: %v", f.policy, err)
		}
		if unknown := config.UnknownScalingPolicyKeys(string(b)); len(unknown) > 0 {
			return opts, usagef("--policy %s: keys a scaling policy does not have: %s", f.policy, strings.Join(unknown, ", "))
		}
		opts.Policy = &p
	}
	prom, err := f.client(warn)
	if err != nil {
		return opts, err
	}
	opts.Prometheus = prom
	opts.Namespace = f.namespace
	opts.From, opts.To, opts.Step = from, to, f.step
	opts.KubeStateMetrics = f.ksm
	opts.BatchCap, opts.GPUsPerReplica, opts.Accelerator = f.batchCap, f.gpus, f.accelerator
	opts.Force = f.force
	return opts, nil
}

// hasFlag reports whether args sets flag, as --flag=value or --flag value.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// parseTime reads RFC 3339 or Unix seconds, fractions allowed, to the
// millisecond Prometheus keeps: a finer instant is truncated.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Truncate(time.Millisecond), nil
	}
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, fmt.Errorf("%q is neither RFC 3339 nor Unix seconds", s)
	}
	return time.UnixMilli(int64(sec * 1000)).UTC(), nil
}

// client is the Prometheus API: TLS with the CA file or without
// verification, a timeout on every request, and the bearer token on https
// requests to Prometheus' own host only, as the controller sends it
// (internal/prometheus). A bearer token for an http Prometheus is refused:
// it would never be sent.
func (f captureFlags) client(warn io.Writer) (promv1.API, error) {
	u, err := url.Parse(f.prometheus)
	if err != nil || (u.Scheme != "http" && u.Scheme != schemeHTTPS) || u.Host == "" {
		return nil, usagef("--prometheus %q is not an http or https URL", f.prometheus)
	}
	if u.Scheme != schemeHTTPS {
		switch {
		case f.tokenFile != "":
			return nil, usagef("--bearer-token-file is sent to an https Prometheus only, and %s is %s: use https, or drop the token", f.prometheus, u.Scheme)
		case f.caFile != "" || f.insecure:
			return nil, usagef("--ca-file and --insecure-skip-verify are for an https Prometheus, and %s is %s", f.prometheus, u.Scheme)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if u.Scheme == schemeHTTPS {
		if f.insecure {
			fmt.Fprintf(warn, "whatif: warning: --insecure-skip-verify: %s's certificate is not verified; "+
				"anyone on the path can read and change what the capture reads\n", u.Host)
		}
		tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: f.insecure} //nolint:gosec // asked for by --insecure-skip-verify
		if f.caFile != "" {
			pem, err := os.ReadFile(f.caFile)
			if err != nil {
				return nil, usageError{err}
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, usagef("--ca-file %s holds no PEM certificate", f.caFile)
			}
			tc.RootCAs = pool
		}
		transport.TLSClientConfig = tc
	}
	var rt http.RoundTripper = transport
	if f.tokenFile != "" {
		b, err := os.ReadFile(f.tokenFile)
		if err != nil {
			return nil, usageError{err}
		}
		token := strings.TrimSpace(string(b))
		if token == "" {
			return nil, usagef("--bearer-token-file %s holds no token", f.tokenFile)
		}
		rt = bearer{base: transport, host: u.Host, token: token}
	}
	c, err := api.NewClient(api.Config{Address: f.prometheus, Client: &http.Client{Transport: rt, Timeout: f.timeout}})
	if err != nil {
		return nil, usageError{err}
	}
	return promv1.NewAPI(c), nil
}

// bearer adds the token to https requests to Prometheus' own host only, so
// neither a redirect to plain http nor one to another host carries it. The
// HTTP client drops an Authorization header on a redirect to another host,
// but this round tripper sets it on every request it sends, the redirected
// ones included, so it checks the host itself.
type bearer struct {
	base  http.RoundTripper
	host  string
	token string
}

// RoundTrip implements http.RoundTripper: req with the token when it goes
// over https to Prometheus' host (compared without case, as a host name
// is), req as it is otherwise.
func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == schemeHTTPS && strings.EqualFold(req.URL.Host, b.host) {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(req)
}
