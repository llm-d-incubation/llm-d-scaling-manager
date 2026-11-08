package prefill

import (
	"math"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// TransferWindowCycles is how many cycles a TransferWindow averages. It is the
// same count the prompt window keeps (whatif.PromptWindowCycles).
const TransferWindowCycles = 20

// transferRead is one cycle's mean transfer time, in seconds.
type transferRead struct {
	at      time.Time
	seconds float64
}

// TransferWindow is what a decode variant's rows say of the KV transfer. S1 and
// the what-if CLI read it alike, so one recording prices one figure.
//
// A pod behind on its pulls reads ten times what it reads quiet, so a cycle counts
// as quiet only when every row with a reading has an empty queue: one quiet pod
// beside busy ones says nothing of the fleet. Readings age out at HeldDecodeAge,
// the age a held decode TTFT may reach, because this figure sets the old sum's
// floor under the priced TTFT, and a quiet figure from long ago must not lower it.
// Past that the fleet's every-row figure stands, which reads high, and with no
// reading at all there is none: the caller's TransferOmitted.
type TransferWindow struct {
	quiet, all        []transferRead
	now               time.Time
	readThisCycle     bool
	accelerator, args string
}

// TransferReading is a TransferWindow at its latest cycle.
type TransferReading struct {
	// Quiet is the mean over the window's quiet cycles, and AllRows over every
	// cycle with a reading, in seconds; 0 with none in age.
	Quiet, AllRows float64
	// Held says Quiet comes from earlier cycles: the latest one was not quiet.
	Held bool
}

// Figure is the transfer to price: the quiet one, else every row's, else 0.
func (r TransferReading) Figure() float64 {
	if r.Quiet > 0 {
		return r.Quiet
	}
	return r.AllRows
}

// Under says what the next readings are taken under: a change of accelerator or
// of engine arguments sweeps the window, as DecodeTermWindow.Under does. An
// unknown value changes nothing.
func (w *TransferWindow) Under(accelerator, args string) {
	moved := accelerator != "" && w.accelerator != "" && accelerator != w.accelerator ||
		args != "" && w.args != "" && args != w.args
	if accelerator != "" {
		w.accelerator = accelerator
	}
	if args != "" {
		w.args = args
	}
	if moved {
		w.quiet, w.all = nil, nil
	}
}

// Observe takes one cycle's rows of one decode variant.
func (w *TransferWindow) Observe(now time.Time, rows []domain.ReplicaMetrics) {
	w.now, w.readThisCycle = now, false
	var sum float64
	n, queued := 0, false
	for _, rm := range rows {
		if !Serving(rm) || !rm.AvgKVTransferTimeKnown || math.IsNaN(rm.AvgKVTransferTime) ||
			math.IsInf(rm.AvgKVTransferTime, 0) || !(rm.AvgKVTransferTime > 0) {
			continue
		}
		sum += rm.AvgKVTransferTime
		n++
		queued = queued || rm.QueueLength > 0
	}
	if n > 0 {
		read := transferRead{at: now, seconds: sum / float64(n)}
		w.all = keepTransfer(w.all, read)
		if !queued {
			w.quiet = keepTransfer(w.quiet, read)
			w.readThisCycle = true
		}
	}
	w.quiet, w.all = aged(w.quiet, now), aged(w.all, now)
}

// Reading is the window as of its latest cycle.
func (w *TransferWindow) Reading() TransferReading {
	return TransferReading{Quiet: meanRead(w.quiet), AllRows: meanRead(w.all),
		Held: len(w.quiet) > 0 && !w.readThisCycle}
}

// keepTransfer is reads with r appended, holding the last TransferWindowCycles.
func keepTransfer(reads []transferRead, r transferRead) []transferRead {
	reads = append(reads, r)
	if len(reads) > TransferWindowCycles {
		reads = reads[len(reads)-TransferWindowCycles:]
	}
	return reads
}

// aged is reads less those older than HeldDecodeAge at now.
func aged(reads []transferRead, now time.Time) []transferRead {
	for len(reads) > 0 && now.Sub(reads[0].at) > HeldDecodeAge {
		reads = reads[1:]
	}
	return reads
}

// meanRead is the mean of reads, 0 with none.
func meanRead(reads []transferRead) float64 {
	if len(reads) == 0 {
		return 0
	}
	var sum float64
	for _, r := range reads {
		sum += r.seconds
	}
	return sum / float64(len(reads))
}
