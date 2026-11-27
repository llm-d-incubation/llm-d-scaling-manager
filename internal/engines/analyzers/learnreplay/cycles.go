package learnreplay

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// CyclesOf is the recording src opens with only the cycles keep keeps, so
// that a replay of it learns from those cycles alone: the fit of a backtest
// split by load (whatifapi.LoadSplitOf), read from the steps below a rate.
//
// A cycle is the events from one slo BeginCycle to the next, and keep is
// asked once a cycle with the times of its saturation Analyze events, the
// times its series samples carry (CycleSample.At). A BeginCycle followed by
// no Analyze before the next one (a catch-up after failed cycles) joins the
// cycle that follows it. The events before the first BeginCycle, the start
// line among them, are always kept, and a last cycle with no Analyze is kept
// as the one before it was. The events kept are renumbered from 1, as a
// recording's must be (learnrec.Read), and are otherwise the lines as
// written.
func CyclesOf(src Source, keep func(at []time.Time) bool) Source {
	return func() (io.ReadCloser, error) {
		rc, err := src()
		if err != nil {
			return nil, err
		}
		pr, pw := io.Pipe()
		go func() {
			err := copyCycles(rc, pw, keep)
			_ = rc.Close()
			_ = pw.CloseWithError(err)
		}()
		return pr, nil
	}
}

// copyCycles copies the recording r reads to w with only the cycles keep
// keeps (CyclesOf).
func copyCycles(r io.Reader, w io.Writer, keep func(at []time.Time) bool) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	var seq uint64
	write := func(ev learnrec.RawEvent) error {
		seq++
		line, err := renumbered(ev, seq)
		if err != nil {
			return err
		}
		_, err = bw.Write(line)
		return err
	}
	var block []learnrec.RawEvent
	var at []time.Time
	opened, last := false, true
	flush := func() error {
		if len(at) > 0 {
			last = keep(at)
		}
		if last {
			for _, ev := range block {
				if err := write(ev); err != nil {
					return err
				}
			}
		}
		block, at = block[:0], at[:0]
		return nil
	}
	s := learnrec.NewScanner(r)
	for s.Scan() {
		ev := s.Event()
		switch {
		case ev.Kind == slo.KindBeginCycle && len(at) > 0:
			if err := flush(); err != nil {
				return err
			}
		case ev.Kind == slo.KindBeginCycle:
			opened = true
		case !opened:
			if err := write(ev); err != nil {
				return err
			}
			continue
		}
		block = append(block, ev)
		if ev.Kind == saturation.KindAnalyze {
			at = append(at, ev.Now)
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	return bw.Flush()
}

// seqPrefix opens every line Marshal writes.
const seqPrefix = `{"Seq":`

// renumbered is ev's line with the sequence number seq: the line as written
// with its number replaced, or, for a line Marshal did not write, the line
// Marshal writes for it.
func renumbered(ev learnrec.RawEvent, seq uint64) ([]byte, error) {
	if rest, ok := strings.CutPrefix(ev.Line, seqPrefix); ok {
		if i := strings.IndexByte(rest, ','); i > 0 {
			if _, err := strconv.ParseUint(rest[:i], 10, 64); err == nil {
				out := make([]byte, 0, len(ev.Line)+8)
				out = append(out, seqPrefix...)
				out = strconv.AppendUint(out, seq, 10)
				out = append(out, rest[i:]...)
				return append(out, '\n'), nil
			}
		}
	}
	return learnrec.Line(seq, ev.Now, ev.Kind, ev.Data)
}
