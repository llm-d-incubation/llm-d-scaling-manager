package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// recordingSource is a recording read in passes, never held whole: a file,
// opened once so that every pass reads the same bytes and gunzipped as it is
// read when it is gzip, or a recording made in memory (--prometheus, or a
// --recording that is no regular file, such as a pipe). A day-long recording
// is a gigabyte of lines; each pass holds one.
type recordingSource struct {
	open learnreplay.Source
	// file is the file under open, path its name and info its state when it
	// was opened; nil for a recording in memory.
	file *os.File
	path string
	info os.FileInfo
	// sum is the SHA-256 of the gunzipped recording, set by check.
	sum [sha256.Size]byte
	// passes counts the passes opened, and onPass, when set, is called with
	// the count as each opens: a test's hook to change the file between
	// passes.
	passes int
	onPass func(pass int)
}

// memoryRecording is the source of a recording held in memory.
func memoryRecording(data []byte) *recordingSource {
	return &recordingSource{open: learnreplay.BytesSource(data)}
}

// openRecording is the source of the recording file at path. A file that is
// not a regular file (a pipe, as <(cat x.gz) gives) has no size to read in
// passes: it is read whole into memory, gunzipped, as before passes. A
// directory is its read's error, which names the path once.
//
// With regularOnly (the server), anything but a regular file is refused,
// judged by the file opened, not by its path: a name checked as a regular
// file can be swapped for a link to /dev/zero or a pipe before it is
// opened, and read whole, that is unbounded memory or a request that never
// ends. It is opened without waiting for a pipe's writer (openNonBlock), so
// that a pipe is refused, not waited on.
func openRecording(path string, regularOnly bool) (*recordingSource, error) {
	flag := os.O_RDONLY
	if regularOnly {
		flag |= openNonBlock
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() && regularOnly {
		_ = f.Close()
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if !fi.Mode().IsRegular() {
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		if data, err = gunzipped(data); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return memoryRecording(data), nil
	}
	size := fi.Size()
	var magic [2]byte
	n, _ := f.ReadAt(magic[:], 0)
	gz := n == len(magic) && bytes.Equal(magic[:], []byte{0x1f, 0x8b})
	s := &recordingSource{file: f, path: path, info: fi}
	s.open = func() (io.ReadCloser, error) {
		s.passes++
		if s.onPass != nil {
			s.onPass(s.passes)
		}
		r := io.NewSectionReader(f, 0, size)
		if !gz {
			return io.NopCloser(readErrors{r}), nil
		}
		z, err := gzip.NewReader(bufio.NewReaderSize(r, 1<<20))
		if err != nil {
			return nil, readError{err}
		}
		return struct {
			io.Reader
			io.Closer
		}{readErrors{z}, z}, nil
	}
	return s, nil
}

// openRecordingCycles is openRecording's source of the file at path, read
// with only the cycles the set holds where it is not nil
// (learnreplay.CyclesOf).
func openRecordingCycles(path string, cycles *whatifapi.CycleSet) (*recordingSource, error) {
	src, err := openRecording(path, false)
	if err != nil || cycles == nil {
		return src, err
	}
	src.open = learnreplay.CyclesOf(src.open, cycles.Keeps)
	return src, nil
}

// Close closes the file under the source, if any.
func (s *recordingSource) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	return s.file.Close()
}

// unchanged is an error when the file under the source changed after it was
// opened: its size or modification time, or the path now names another
// file. Every pass reads the size fixed at open, so a file rewritten in
// place reads short or mixed in a later pass, under the sum of the first:
// the caller reads it after the last pass and keeps nothing on an error. A
// recording in memory cannot change.
func (s *recordingSource) unchanged() error {
	if s == nil || s.file == nil {
		return nil
	}
	if now, err := s.file.Stat(); err == nil && sameVersion(s.info, now) {
		if at, err := os.Stat(s.path); err == nil && os.SameFile(s.info, at) && sameVersion(s.info, at) {
			return nil
		}
	}
	return usagef("%s: the file changed while it was read: read it again once it is written", s.path)
}

// sameVersion reports whether a and b give a file the same size and
// modification time.
func sameVersion(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// readError is an error reading the recording's bytes (a file, or its gzip),
// not a line of it: the file's error, as reading it whole gave it.
type readError struct{ err error }

func (e readError) Error() string { return e.err.Error() }
func (e readError) Unwrap() error { return e.err }

// readErrors is a reader whose errors, but io.EOF, are readErrors.
type readErrors struct{ r io.Reader }

func (r readErrors) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = readError{err}
	}
	return n, err
}

// check reads every line of the recording, as learnrec.Read would, and sums
// its bytes: a file that does not read is a usage error naming path, a
// recording whose lines do not read or that has no start line one naming
// name, as answers read them.
func (s *recordingSource) check(name, path string) error {
	rc, err := s.open()
	if err != nil {
		return usagef("%s: %v", path, err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	tee := io.TeeReader(rc, h)
	sc := learnrec.NewScanner(tee)
	events, start := 0, false
	for sc.Scan() {
		if events == 0 {
			start = sc.Event().Kind == learnrec.KindStart
		}
		events++
	}
	// The rest is read whatever stopped the scan: what follows a torn tail
	// is summed too, and a file that does not read is that error, before
	// any of its lines', as reading it whole gave it.
	_, rest := io.Copy(io.Discard, tee)
	var re readError
	switch err := sc.Err(); {
	case errors.As(err, &re):
		return usagef("%s: %v", path, re.err)
	case errors.As(rest, &re):
		return usagef("%s: %v", path, re.err)
	case err != nil:
		return usagef("%s: not a learner recording: %v", name, err)
	case rest != nil:
		return usagef("%s: %v", path, rest)
	case !start:
		return usagef("%s: not a learner recording: no start line", name)
	}
	copy(s.sum[:], h.Sum(nil))
	return nil
}
