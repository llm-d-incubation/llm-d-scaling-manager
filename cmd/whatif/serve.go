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
	"strconv"
	"time"

	"github.com/go-logr/logr"
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
	fs.StringVar(&f.window.namespace, "namespace", "", "the namespace a prometheus source reads when the request names none")
	fs.IntVar(&f.window.batchCap, "batch-cap", 0, "the engine's batch cap a window is captured with, as capture --batch-cap")
	fs.StringVar(&f.recordings, "recordings", "", "a directory of learner recordings a recording source names by file name")
	fs.StringVar(&f.listen, "listen", "127.0.0.1:8080", "the address to serve on; a non-loopback one needs --auth=kubernetes")
	fs.StringVar(&f.auth, "auth", whatifapi.AuthNone, "none (loopback only) or kubernetes (a bearer token checked with TokenReview and SubjectAccessReview)")
	fs.StringVar(&f.tlsCert, "tls-cert-file", "", "the server's certificate, PEM; required with --auth=kubernetes")
	fs.StringVar(&f.tlsKey, "tls-key-file", "", "the server's private key, PEM; required with --auth=kubernetes")
	fs.DurationVar(&f.shutdownDeadline, "shutdown-timeout", 10*time.Second, "how long requests in flight may run once the server is told to stop")
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
	srv := &http.Server{Addr: f.listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		return err
	}
	if f.auth == whatifapi.AuthKubernetes {
		cert, err := tls.LoadX509KeyPair(f.tlsCert, f.tlsKey)
		if err != nil {
			return usagef("--tls-cert-file, --tls-key-file: %v", err)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
		ln = tls.NewListener(ln, srv.TLSConfig)
	}
	fmt.Fprintf(warn, "whatif: serving the capacity planner's API on %s (auth %s)\n", ln.Addr(), f.auth)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), f.shutdownDeadline)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
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
	srv := whatifapi.New(whatifapi.Config{Resolver: &servedSources{window: f.window, recordings: f.recordings, warn: warn},
		WhatifSchema: "schema-v" + strconv.Itoa(schemaVersion)})
	h := srv.Handler()
	if f.auth != whatifapi.AuthKubernetes {
		return h, nil
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
	return whatifapi.Authenticate(c, h), nil
}
