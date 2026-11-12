package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A key pair that does not load leaves the port free: serve reads it before
// it listens.
func TestServeBadKeyPairLeavesNoListener(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`), 0o600))
	t.Setenv("KUBECONFIG", kubeconfig)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	err = run(context.Background(), []string{"serve", "--recordings", dir, "--listen", addr, "--auth", "kubernetes",
		"--tls-cert-file", filepath.Join(dir, "none.crt"), "--tls-key-file", filepath.Join(dir, "none.key")}, io.Discard, io.Discard)
	var u usageError
	require.ErrorAs(t, err, &u)
	assert.Contains(t, err.Error(), "--tls-cert-file")

	again, err := net.Listen("tcp", addr)
	require.NoError(t, err, "the failed serve left its listener open")
	require.NoError(t, again.Close())
}

// A request in flight at shutdown has its context ended at the deadline, so
// that a plan stops at its next figure and the server stops cleanly within
// the grace.
func TestServeEndsRequestsAtTheDeadline(t *testing.T) {
	started, ended := make(chan struct{}), make(chan error, 1)
	srv := newHTTPServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		ended <- r.Context().Err()
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveUntil(ctx, srv, ln, 100*time.Millisecond) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	stop()
	select {
	case err := <-done:
		assert.NoError(t, err, "the server did not stop within the grace")
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	select {
	case err := <-ended:
		assert.True(t, errors.Is(err, context.Canceled), "%v", err)
	default:
		t.Fatal("the request's context did not end")
	}
}
