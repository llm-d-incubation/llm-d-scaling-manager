package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

func runGenerate(ctx context.Context, args []string, out, _ io.Writer) error {
	fs := newFlags("generate", out)
	in := fs.String("in", "", "the capture (capacity-planner capture -o), gzipped or not (required)")
	dst := fs.String("o", "", "where to write the recording; a name ending in .gz is gzipped (required)")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if *in == "" || *dst == "" {
		return usagef("generate needs -in and -o")
	}
	if err := generate(ctx, *in, *dst, out); err != nil {
		// The one place generate's errors get their prefix.
		var u usageError
		if errors.As(err, &u) {
			return usageError{fmt.Errorf("generate: %w", u.error)}
		}
		return fmt.Errorf("generate: %w", err)
	}
	return nil
}

// generate plays the capture at in and writes the recording to dst: a
// usage error for a capture it cannot read, any other error for a failure
// that is not the capture's.
func generate(ctx context.Context, in, dst string, out io.Writer) error {
	capture, err := readMaybeGzipped(in)
	if err != nil {
		return usageError{err}
	}
	rep, err := generateRecording(ctx, capture)
	if err != nil {
		return err
	}
	recording := rep.Recording
	start, err := startOf(recording)
	if err != nil {
		return fmt.Errorf("the generated recording: %w", err)
	}
	if strings.HasSuffix(dst, ".gz") {
		if recording, err = learnreplay.Gzip(recording); err != nil {
			return fmt.Errorf("gzip the recording: %w", err)
		}
	}
	if err := writeFileAtomic(dst, func(w io.Writer) error {
		_, err := w.Write(recording)
		return err
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "generated %d steps into %s: %d bytes, analyzes %v\n", rep.Applied, dst, len(recording), rep.Analyzes)
	for _, c := range start.Caveats {
		fmt.Fprintf(out, "  caveat: %s\n", c)
	}
	return nil
}

// generateRecording plays a capture, gunzipped, through the analyzers
// (learnreplay, Generate mode): the one place both generate and the answer
// commands' --prometheus mode turn a capture into a recording. A capture
// Generate cannot read is a usage error; any other failure is not the
// capture's.
func generateRecording(ctx context.Context, capture []byte) (*learnreplay.Report, error) {
	rep, err := learnreplay.Replay(ctx, capture, learnreplay.Options{Mode: learnreplay.Generate})
	var bad *learnreplay.CaptureError
	switch {
	case errors.As(err, &bad):
		return nil, usageError{err}
	case err != nil:
		return nil, err
	}
	return rep, nil
}

// startOf is a recording's start line.
func startOf(recording []byte) (learnrec.Start, error) {
	var start learnrec.Start
	read, err := learnrec.Read(recording)
	if err != nil {
		return start, err
	}
	if len(read.Events) == 0 || read.Events[0].Kind != learnrec.KindStart {
		return start, errors.New("no start line")
	}
	if err := read.Events[0].DecodeData(&start); err != nil {
		return start, fmt.Errorf("start line: %w", err)
	}
	return start, nil
}

// outputMode is the mode of every file capacity-planner writes: a capture or a
// recording holds a cluster's names, so only its owner reads it.
const outputMode = 0o600

// writeFileAtomic writes path through write, to a temporary file beside it
// renamed over it once write succeeds, so a failed or cancelled run leaves
// the file that was there before.
func writeFileAtomic(path string, write func(w io.Writer) error) (err error) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return usageError{err}
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(outputMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// readMaybeGzipped reads a file, gunzipping it when it is gzipped
// (gunzipped); a gzip error names the file.
func readMaybeGzipped(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if b, err = gunzipped(b); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return b, nil
}
