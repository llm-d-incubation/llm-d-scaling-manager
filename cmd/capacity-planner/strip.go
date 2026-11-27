package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// strip-latency writes a recording with nothing in it but the load: what a
// user shares so that a prediction is made from the traffic and the fleet
// alone (capacity-planner predict). It keeps the start line and each
// cycle's input (saturation.Analyze's Data.Input: request rates, lengths,
// replicas, the engines' settings), with every measured latency taken out
// of it, and drops every other event and every analyzer output: those
// carry figures learned from latency (ITL lines, SLO attainment,
// capacities). The router's rate window is kept: it is no latency, and the
// prediction prices each step over it.

// latencyKeys are the keys a cycle's input carries a measured latency
// under, at any depth: each engine row's TTFT, inter-token, service, queue,
// prefill and KV-transfer times, means and histograms, with their presence
// bits, and the router's histograms, its request processing and
// flow-control wait.
var latencyKeys = map[string]bool{
	"TTFTBuckets": true, "TPOTBuckets": true, "QueueTimeBuckets": true, "FlowControlWaitBuckets": true,
	"TTFTSumRate": true, "TTFTCountRate": true, "TPOTSumRate": true, "TPOTCountRate": true,
	"ProcessingBuckets": true, "ProcessingSumRate": true, "ProcessingCountRate": true,
	"AvgTTFT": true, "AvgTTFTKnown": true,
	"AvgITL": true, "AvgITLKnown": true, "AvgITLEmpty": true,
	"AvgServiceTime": true, "AvgServiceTimeKnown": true,
	"AvgQueueTime": true, "AvgQueueTimeKnown": true,
	"AvgPrefillTime": true, "AvgPrefillTimeKnown": true,
	"AvgKVTransferTime": true, "AvgKVTransferTimeKnown": true,
}

// keptKinds are the events a stripped recording keeps.
var keptKinds = map[string]bool{"start": true, "saturation.Analyze": true}

// runStripLatency is `capacity-planner strip-latency`.
func runStripLatency(_ context.Context, args []string, out, warn io.Writer) error {
	fs := newFlags("strip-latency", out)
	in := fs.String("in", "", "the recording to strip, gzipped or not (required)")
	dst := fs.String("o", "", "where to write it; a name ending in .gz is gzipped (required)")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if *in == "" || *dst == "" {
		return usagef("-in and -o are required")
	}
	f, err := os.Open(*in)
	if err != nil {
		return usageError{err}
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return usageError{fmt.Errorf("%s: %w", *in, err)}
		}
		defer func() { _ = zr.Close() }()
		r = zr
	}
	var st stripStats
	err = writeFileAtomic(*dst, func(w io.Writer) error {
		var err error
		st, err = stripLatency(r, w, strings.HasSuffix(*dst, ".gz"))
		return err
	})
	if err != nil {
		return err
	}
	if st.torn {
		fmt.Fprintln(warn, progName+": strip-latency: the last line was cut off mid-write and is left out, as the planner reads it")
	}
	fmt.Fprintf(out, "wrote %s: the start line and %d cycles kept, %d other events dropped, every measured latency taken out\n",
		*dst, max(st.kept-1, 0), st.dropped)
	return nil
}

// stripStats is what a strip kept and dropped, and whether the last line
// was cut off.
type stripStats struct {
	kept, dropped int
	torn          bool
}

// stripLatency copies the start line and each cycle's input of the
// recording r to w, every latencyKeys key removed at any depth, the router's
// latency down to its rate window, and the events renumbered from 1; gzipped
// when gz. A line that is not JSON is an error, but for the last, which a
// recording cut off mid-write has (the planner drops it too).
func stripLatency(r io.Reader, w io.Writer, gz bool) (stripStats, error) {
	var st stripStats
	dst := w
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(w)
		dst = zw
	}
	bw := bufio.NewWriter(dst)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	line := 0
	var pending error
	for sc.Scan() {
		line++
		if pending != nil {
			return st, pending
		}
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var ev map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&ev); err != nil {
			pending = usagef("line %d: not JSON: %v", line, err)
			continue
		}
		kind, _ := ev["Kind"].(string)
		if !keptKinds[kind] {
			st.dropped++
			continue
		}
		if kind != "start" {
			data, _ := ev["Data"].(map[string]any)
			input, ok := data["Input"].(map[string]any)
			if !ok {
				return st, usagef("line %d: a %s event with no input", line, kind)
			}
			if rl, ok := input["RouterLatency"].(map[string]any); ok {
				// The router's window is what the latency will be measured
				// over, where it measures TTFT; else the engines' minute is.
				// The prefix producers' cached tokens and the per-endpoint
				// load are no latency: they stay, for the prefix
				// duplication and the load across Pods.
				kept := map[string]any{}
				if b, _ := rl["TTFTBuckets"].([]any); len(b) > 0 {
					kept["WindowSeconds"] = rl["WindowSeconds"]
				}
				for _, k := range []string{"PrefixPredictedTokenRate", "PrefixBestAvailableTokenRate", "Endpoints", "PrefixProducers"} {
					if v, ok := rl[k]; ok {
						kept[k] = v
					}
				}
				if len(kept) > 0 {
					input["RouterLatency"] = kept
				} else {
					delete(input, "RouterLatency")
				}
			}
			ev["Data"] = map[string]any{"Input": dropLatency(input)}
		}
		st.kept++
		ev["Seq"] = st.kept
		b, err := json.Marshal(ev)
		if err != nil {
			return st, err
		}
		if _, err := bw.Write(append(b, '\n')); err != nil {
			return st, err
		}
	}
	if err := sc.Err(); err != nil {
		return st, usagef("line %d: %v", line+1, err)
	}
	st.torn = pending != nil
	if st.kept < 2 {
		return st, usagef("no cycle input (saturation.Analyze) to keep: give a learner recording (capacity-planner generate turns a capture into one)")
	}
	if err := bw.Flush(); err != nil {
		return st, err
	}
	if zw != nil {
		return st, zw.Close()
	}
	return st, nil
}

// dropLatency is v with every latencyKeys key removed, at any depth.
func dropLatency(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if latencyKeys[k] {
				delete(x, k)
				continue
			}
			x[k] = dropLatency(e)
		}
	case []any:
		for i, e := range x {
			x[i] = dropLatency(e)
		}
	}
	return v
}
