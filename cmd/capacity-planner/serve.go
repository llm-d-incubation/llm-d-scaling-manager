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
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

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
	// allowedNamespaces is --allowed-namespaces, comma-separated, and
	// audiences --token-audiences.
	allowedNamespaces, audiences string
}

// runServe serves the capacity planner's API (internal/whatifapi) until ctx
// ends: POST /v1/inputs and POST /v1/plan, priced on the CLI's own code
// paths. A Prometheus window is read from --prometheus with capture's
// flags, the window and namespace coming with each request; a recording is
// read from --recordings by file name. It binds a loopback address unless
// --auth=kubernetes, which needs TLS.
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
	fs.StringVar(&f.allowedNamespaces, "allowed-namespaces", "", "the namespaces a prometheus source may read, comma-separated; default --namespace alone")
	fs.StringVar(&f.audiences, "token-audiences", "", "with --auth=kubernetes, the audiences a TokenReview checks a token for, comma-separated; default the API server's")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	handler, err := f.handler(fs, warn)
	if err != nil {
		return err
	}
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
	return serveUntil(ctx, srv, ln, f.shutdownDeadline)
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
	})
	if err != nil {
		return nil, err
	}
	if err := whatifapi.CheckListen(f.listen, f.auth); err != nil {
		return nil, usageError{err}
	}
	if f.auth == whatifapi.AuthKubernetes && (f.tlsCert == "" || f.tlsKey == "") {
		return nil, usagef("--auth=%s needs --tls-cert-file and --tls-key-file: a bearer token is not sent in the clear", whatifapi.AuthKubernetes)
	}
	if f.recordings == "" && f.window.prometheus == "" {
		return nil, usagef("serve needs --prometheus, --recordings or both: a source to resolve inputs from")
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
	src := &servedSources{window: f.window, recordings: f.recordings, warn: warn, allowed: allowed}
	srv := whatifapi.New(whatifapi.Config{Resolver: src, WhatifSchema: "schema-v" + strconv.Itoa(schemaVersion)})
	h := srv.Handler()
	if f.auth != whatifapi.AuthKubernetes {
		return whatifapi.LoopbackOnly(h), nil
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("--auth=%s: no Kubernetes client: %w", whatifapi.AuthKubernetes, err)
	}
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("--auth=%s: %w", whatifapi.AuthKubernetes, err)
	}
	a := whatifapi.NewAuthenticator(c, splitList(f.audiences), 0, func(format string, args ...any) {
		fmt.Fprintf(warn, format+"\n", args...)
	})
	src.namespaceAllowed = a.NamespaceAllowed
	return a.Wrap(h), nil
}

// newHTTPServer is the server on addr, with its timeouts: a client may not
// hold a connection open by sending slowly, and a response may take as long
// as a cold plan.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: readTimeout,
		WriteTimeout: writeTimeout, IdleTimeout: idleTimeout}
}

// The server's timeouts. A write may run as long as a cold plan, or a capture
// of a Prometheus window, takes.
const (
	readTimeout  = 30 * time.Second
	writeTimeout = 10 * time.Minute
	idleTimeout  = 2 * time.Minute
)

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
