// Command capacity-planner is the capacity planner's command line
// (docs/proposals/slo-capacity-planner.md, "CLI reference"). It captures a
// past window from Prometheus, turns a capture into a learner recording, and
// answers the planner's two questions from a recording, replayed through the
// analyzers to the cycle asked for:
//
//	capacity-planner capture         --prometheus URL --namespace NS (--kube-state-metrics | --role-of ROLE=SELECTOR ...)
//	                                 --from T0 --to T1 [--step 15s] [--policy FILE] -o run.capture.jsonl.gz
//	capacity-planner generate        -in run.capture.jsonl.gz -o run.jsonl
//	capacity-planner freeze          --recording FILE --spec FILE [-o FILE] [--targets FILE] [--ca2 BUCKET=CA2]
//	capacity-planner replicas-to-slo --recording FILE [--cycle N|last] [--model M] [--decode N]
//	                                 [--headroom H | --rate R] [--tpot-percentile P [--assume-tpot-ratio R]]
//	                                 [--ttft-percentile P] [--prefill N] [--prefill-variant V] [--assume-vp TOKENS]
//	                                 [--assume-a D] [--assume-prompt-cv CV] [--arrival-cv2 C] [--flow-control on|off] [--assume-router-hop D] [-o json]
//	capacity-planner slo-to-rate     --recording FILE [--cycle N|last] [--model M] [--decode N]
//	                                 --tpot 4ms[@p90] [--assume-tpot-ratio R] [--headroom H]
//	                                 [--ttft 200ms[@p95]] [--prefill N] [--prefill-variant V] [--assume-vp TOKENS]
//	                                 [--assume-a D] [--assume-prompt-cv CV] [--arrival-cv2 C] [--flow-control on|off] [--assume-router-hop D] [-o json]
//	capacity-planner joint-ttft      --recording FILE [--cycle N|last] [--model M] [--prefill N] [--decode N]
//	                                 [--rate R] [--prompt I] [--hop D] [-o json]
//	capacity-planner replicas-to-slo|slo-to-rate --prometheus URL --namespace NS (--kube-state-metrics | --role-of ROLE=SELECTOR ...)
//	                                 --from T0 --to T1 [capture's flags] [the question's flags]
//	capacity-planner at-replicas     --recording FILE [--cycle N|last] [--model M] [--prefill N] [--decode N]
//	                                 [--shape S ...] [--load L ...] [--load-statistic mean|p95|peak | --load-seen R] [--load-seen-x X]
//	                                 [--ttft 2s@p90] [--tpot 30ms] [--headroom H] [--ttft-percentile 50,90] [--tpot-percentile 90]
//	                                 [--arrival-cv2 C] [the --assume-* flags] [-o json]
//	capacity-planner day-plan        (--recording FILE | --prometheus URL capture's flags --namespace NS [--from T0 --to T1])
//	                                 [--bucket 5m] [--size-for mean|p95|max] [--ttft 500ms@p95] [--tpot 4ms] [-o json]
//	capacity-planner serve           [--recordings DIR] [--prometheus URL capture's flags --namespace NS [--allowed-namespaces NS,...]]
//	                                 [--listen 127.0.0.1:8080] [--auth none|kubernetes --tls-cert-file F --tls-key-file F [--token-audiences A,...]]
//	                                 [--listen-all-in-container (in a container: --listen 0.0.0.0:PORT, no auth)]
//	                                 [--allow-upload [--upload-max-size 256Mi]]
//	capacity-planner backtest        (--recording FILE [--split 60% | --fit-until T] | --fit FILE --test FILE)
//	                                 [--step 5m] [--settle 2m] [--ttft-target 500ms@p90] [--arrival-cv2 C]
//	                                 [--measured auto|router|engine] [--assume-vp V] [--assume-a D] [--assume-prompt-cv CV]
//	                                 [--assume-router-hop D] [--assume-itl-b B|floor] [--flow-control on|off]
//	                                 [--router-wait-in-prefill | --price-router-wait] [--allow-config-mismatch] [-o json]
//	capacity-planner strip-latency   -in FILE -o FILE
//	capacity-planner predict         --fit FILE --window FILE -o prediction.json [--csv FILE] [--step 5m] [--settle 2m]
//	                                 [--measure-window 5m] [--model M] [--variant V] [--prefill-variant V]
//	                                 [--arrival-cv2 C] [--assume-vp V] [--assume-a D] [--assume-prompt-cv CV]
//	                                 [--assume-router-hop D] [--assume-itl-b B|floor] [--flow-control on|off]
//	                                 [--router-wait-in-prefill | --price-router-wait] [--allow-config-mismatch]
//	capacity-planner compare         --prediction FILE --measured FILE [--ttft-target 500ms@p90] [--measured-by auto|router|engine]
//	                                 [--model M] [--variant V] [--prefill-variant V] [-o json]
//	capacity-planner version         (the release and commit; the controller of the recordings is released under the same tag)
//
// capture writes the window's cycle inputs (internal/whatifsource);
// generate plays them through the analyzers in the engine's order
// (learnreplay, Generate mode) and writes the recording, which
// learnergolden export pseudonymises and learnergolden add accepts at the
// commit that generated it. replicas-to-slo and slo-to-rate read a
// recording (learnreplay.StateAt): a file, or with --prometheus the
// recording capture and generate would write for the window, made in
// memory along the same path, so that the answer is the files' answer.
//
// A TTFT is asked for by a TTFT flag; with none, the answers are decode's
// alone. It prices a P/D fleet (ttft.go), and prints UNVALIDATED. joint-ttft
// prices the joint prefill and decode model's mean at a fleet (joint.go). Aggregated
// TTFT and per-bucket answers are not built (the planner's M1).
//
// at-replicas judges one fleet at the load the window saw: the utilization
// there, the knees, the SLO against load and a verdict (report.go); POST
// /v1/report answers the same.
//
// backtest fits on one window and predicts another from its inputs alone,
// beside what the fleet measured there (backtest.go, POST /v1/backtest;
// docs/guides/capacity-planner/check.md).
//
// strip-latency, predict and compare are the backtest in two steps: a
// window's load is predicted to a file carrying hashes of its inputs and of
// itself (predict.go, strip.go; POST /v1/predict and /v1/compare), and
// compared later with the same window measured.
//
// serve runs the capacity planner's HTTP API on the same code paths
// (serve.go, internal/whatifapi; docs/developer-guide/capacity-planner-serve.md).
//
// Exit codes: 0 done, and every role answered; 3 a role refused (the
// output is still printed); 2 a usage or input error (a bad flag, an
// unreadable or malformed capture or recording, overlapping --role-of
// selectors, a window too large without --force); 1 anything else (a failed
// Prometheus query, a Generate or a replay that fails inside the analyzers);
// 130 or 143 stopped by SIGINT or SIGTERM (execute).
//
// The command was named whatif before. Run under that name (make
// capacity-planner also builds bin/whatif), it works the same and first
// prints a one-line deprecation note to stderr. whatif will be removed in
// v0.13.0.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// progName is the command's name in its usage, errors and warnings.
const progName = "capacity-planner"

// deprecatedName is the command's old name, still accepted until deprecatedNameRemovedIn.
const deprecatedName = "whatif"

// deprecatedNameRemovedIn is the release that drops deprecatedName.
const deprecatedNameRemovedIn = "v0.13.0"

// deprecationNote is printed to stderr when the command runs as
// deprecatedName.
const deprecationNote = deprecatedName + ": deprecated: the command is now " + progName +
	" (make " + progName + ", bin/" + progName + "); " + deprecatedName + " will be removed in " + deprecatedNameRemovedIn

// invokedAsDeprecated reports whether argv0, the path the command was run
// as, names the old command: whatif, or whatif.exe on Windows, in any case.
func invokedAsDeprecated(argv0 string) bool {
	base := filepath.Base(argv0)
	if len(base) > len(".exe") && strings.EqualFold(base[len(base)-len(".exe"):], ".exe") {
		base = base[:len(base)-len(".exe")]
	}
	return strings.EqualFold(base, deprecatedName)
}

// warnIfDeprecated writes deprecationNote to stderr when argv0 names the
// old command, and nothing otherwise.
func warnIfDeprecated(argv0 string, stderr io.Writer) {
	if invokedAsDeprecated(argv0) {
		fmt.Fprintln(stderr, deprecationNote)
	}
}

func main() {
	warnIfDeprecated(os.Args[0], os.Stderr)
	// The analyzers the replay runs log through controller-runtime: with no
	// logger set it prints a stack trace on a run of 30 s or more.
	ctrllog.SetLogger(logr.Discard())
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr, sigs, stopGrace))
}

// stopGrace is how long a command has, once SIGINT or SIGTERM ends its
// context, to return before the process exits without it. Every command
// reads its context between figures, so this is a backstop for a figure
// that runs long, not the usual way out.
const stopGrace = 5 * time.Second

// selfStopping are the commands that stop on their own clock once their
// context ends: serve gives requests in flight its --shutdown-timeout.
// stopGrace does not cut them; a second signal still does.
var selfStopping = map[string]bool{"serve": true}

// signalName is SIGINT or SIGTERM by that name, any other signal as Go
// names it.
func signalName(sig os.Signal) string {
	switch sig {
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	return sig.String()
}

// execute runs the command args name and returns the process's exit code.
// SIGINT or SIGTERM on sigs ends the command's context; a command that then
// returns a context error, or has not returned within grace (unless it is
// selfStopping), or meets a second signal, is stopped: execute says so on
// warn and returns 128 plus the signal's number (130 for SIGINT, 143 for
// SIGTERM). A command that still returns its own answer or error after the
// signal (serve, shut down cleanly) exits as it would have.
func execute(args []string, out, warn io.Writer, sigs <-chan os.Signal, grace time.Duration) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, out, warn) }()
	var sig os.Signal
	select {
	case err := <-done:
		return finish(err, warn)
	case sig = <-sigs:
	}
	cancel()
	var cut <-chan time.Time
	if len(args) == 0 || !selfStopping[args[0]] {
		t := time.NewTimer(grace)
		defer t.Stop()
		cut = t.C
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			return finish(err, warn)
		}
		fmt.Fprintf(warn, "%s: stopped by %s\n", progName, signalName(sig))
	case <-cut:
		fmt.Fprintf(warn, "%s: stopped by %s: the command did not stop within %s, so it was cut off\n", progName, signalName(sig), grace)
	case s := <-sigs:
		fmt.Fprintf(warn, "%s: stopped by a second signal (%s)\n", progName, signalName(s))
	}
	return signalExitCode(sig)
}

// finish is the exit code for the error a command returned, which it
// prints to warn unless it is an exitStatus, whose output is already
// printed.
func finish(err error, warn io.Writer) int {
	if err == nil {
		return 0
	}
	var s exitStatus
	if !errors.As(err, &s) {
		fmt.Fprintln(warn, progName+":", err)
	}
	return exitCode(err)
}

// signalExitCode is the shell's exit code for a process a signal stopped:
// 128 plus the signal's number, or 1 where a signal has no number.
func signalExitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}

// usageError is a usage or input error: exit 2.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// exitStatus ends a command whose output is already printed with a code
// other than 0 (whatif.ExitCode): nothing more is printed.
type exitStatus int

func (s exitStatus) Error() string { return fmt.Sprintf("exit %d", int(s)) }

// exitCode is the exit code for err: an exitStatus's own, 2 for a usage or
// input error, 1 for anything else.
func exitCode(err error) int {
	var s exitStatus
	if errors.As(err, &s) {
		return int(s)
	}
	var u usageError
	if errors.As(err, &u) {
		return 2
	}
	return 1
}

const usage = "usage: capacity-planner at-replicas|backtest|capture|compare|day-plan|freeze|generate|joint-ttft|predict|replicas-to-slo|serve|slo-to-rate|strip-latency|version [flags]; -h after a command lists its flags"

// commandHelp is what each command answers or does, in the order help lists
// them: the questions first, then the checks, then the tools.
var commandHelp = []struct{ name, what string }{
	{"at-replicas", "judge a fleet of N prefill and decode replicas at the load the window saw (and the loads --load asks): knees, SLO against load, " +
		"a verdict, and how many replicas it needs (the answer to \"how many replicas?\")"},
	{"replicas-to-slo", "the latency a fleet of N replicas gives: its knee, the operating rate below it, and the TPOT " +
		"(and TTFT) there; it does not search for a replica count (at-replicas does)"},
	{"slo-to-rate", "the highest rate a fleet of N replicas carries within a TPOT (and TTFT) target"},
	{"day-plan", "the prefill and decode replicas each stretch of a window needed, beside the ones it had"},
	{"joint-ttft", "the mean TTFT of the joint prefill and decode model at a fleet and a rate (in shadow: nothing sizes from it)"},
	{"backtest", "fit on one window and predict another from its inputs alone, beside what the fleet measured there"},
	{"predict", "a backtest's first step: predict a window to a file"},
	{"compare", "a backtest's second step: compare a prediction file with the window measured"},
	{"strip-latency", "write a recording with every measured latency taken out: the load alone, to share for predict"},
	{"capture", "read a past window from Prometheus into a capture file"},
	{"generate", "turn a capture into a recording, replayed through the analyzers"},
	{"freeze", "write the prediction a TTFT staircase run is scored against, before the run"},
	{"serve", "serve the planner's HTTP API and page"},
	{"version", "print the release and commit"},
}

// helpText is the usage and every command with what it does.
func helpText() string {
	var b strings.Builder
	b.WriteString(usage + "\n\ncommands:\n")
	for _, c := range commandHelp {
		fmt.Fprintf(&b, "  %-16s%s\n", c.name, c.what)
	}
	return b.String()
}

// commandWhat is what the named command does, "" for one commandHelp does
// not list.
func commandWhat(name string) string {
	for _, c := range commandHelp {
		if c.name == name {
			return c.what
		}
	}
	return ""
}

// command is one subcommand: it parses its own flags from args and writes
// what it reports to out, and its warnings to warn (stderr).
type command func(ctx context.Context, args []string, out, warn io.Writer) error

// commands is every subcommand, by name.
var commands = map[string]command{
	"at-replicas":     runAtReplicas,
	"backtest":        runBacktest,
	"compare":         runCompare,
	"predict":         runPredict,
	"strip-latency":   runStripLatency,
	"capture":         runCapture,
	"day-plan":        runDayPlan,
	"freeze":          runFreeze,
	"generate":        runGenerate,
	"joint-ttft":      runJointTTFT,
	"replicas-to-slo": runReplicasToSLO,
	"serve":           runServe,
	"slo-to-rate":     runSLOToRate,
	"version":         runVersion,
}

// run runs one command. Asking for help is not an error. The analyzers a
// replay runs log through controller-runtime, which the CLI never shows; a
// replay past 30 s with no logger set says so on stderr, with a stack.
func run(ctx context.Context, args []string, out, warn io.Writer) error {
	ctrllog.SetLogger(logr.Discard())
	if len(args) == 0 {
		return usageError{errors.New(usage)}
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		if len(args) > 1 && args[1] != "help" {
			cmd, ok := commands[args[1]]
			if !ok {
				return usagef("unknown command %q; %s", args[1], usage)
			}
			return cmd(ctx, []string{"-h"}, out, warn)
		}
		_, err := io.WriteString(out, helpText())
		return err
	}
	cmd, ok := commands[args[0]]
	if !ok {
		return usagef("unknown command %q; %s", args[0], usage)
	}
	return cmd(ctx, args[1:], out, warn)
}

// newFlags is a flag set for one command that reports its errors as usage
// errors and writes its help to out.
func newFlags(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(progName+" "+name, flag.ContinueOnError)
	fs.SetOutput(out)
	if what := commandWhat(name); what != "" {
		fs.Usage = func() {
			fmt.Fprintf(fs.Output(), "%s %s: %s\n\nflags:\n", progName, name, what)
			fs.PrintDefaults()
		}
	}
	return fs
}

// parse parses args; ok is false when the caller should stop: help was
// asked for (err nil) or the flags were wrong (a usage error).
func parse(fs *flag.FlagSet, args []string) (bool, error) {
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return false, nil
	case err != nil:
		return false, usageError{err}
	case fs.NArg() > 0:
		return false, usagef("unexpected argument %q", fs.Arg(0))
	}
	return true, nil
}
