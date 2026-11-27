package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// modelConfigFlag names the served model's config.json (issues #501, #503):
// the attention kind and the MoE class are read from it, and named in the
// engine row and its JSON. Reported, not priced.
const (
	modelConfigFlag  = "model-config"
	modelConfigUsage = "the served model's Hugging Face config.json, or the checkpoint directory holding it: " +
		"its attention kind (full, sliding window, MLA) and MoE class are named beside the engine; reported, not priced. " +
		"With --recording, a config.json beside the recording is read when this is not given"
)

// modelConfigBeside is the file read beside a --recording when
// --model-config is not given: what a capture kit saves with each capture.
const modelConfigBeside = "config.json"

// readModelConfigFlag reads the config --model-config names; the zero
// value where it names none. A file that cannot be read, or that is no
// model config, is the caller's input at fault.
func readModelConfigFlag(path string) (capacity.ModelConfig, error) {
	if path == "" {
		return capacity.ModelConfig{}, nil
	}
	m, err := capacity.ReadModelConfigFile(path)
	if err != nil {
		return m, usagef("--%s: %v", modelConfigFlag, err)
	}
	if m.Error != "" {
		return m, usagef("--%s %s: not a model config: %s", modelConfigFlag, path, m.Error)
	}
	return m, nil
}

// answerModelConfig is the config an answer from a --recording reads: the
// one --model-config names, else the config.json beside the recording,
// else none. The server's recordings (regularOnly) share a directory, so
// nothing is read beside them. A config.json beside the recording that is
// no model config is warned of and left out: it was not asked for.
func (f *answerFlags) answerModelConfig() (capacity.ModelConfig, error) {
	if f.window.modelConfig != "" {
		return readModelConfigFlag(f.window.modelConfig)
	}
	if f.recording == "" || f.regularOnly {
		return capacity.ModelConfig{}, nil
	}
	beside := filepath.Join(filepath.Dir(f.recording), modelConfigBeside)
	if _, err := os.Stat(beside); errors.Is(err, fs.ErrNotExist) {
		return capacity.ModelConfig{}, nil
	}
	m, err := capacity.ReadModelConfigFile(beside)
	switch {
	case err != nil:
		f.warnf("capacity-planner: warning: %v: the model config beside the recording is not read\n", err)
		return capacity.ModelConfig{}, nil
	case m.Error != "":
		f.warnf("capacity-planner: warning: %s is not a model config (%s): not read\n", beside, m.Error)
		return capacity.ModelConfig{}, nil
	}
	return m, nil
}

// warnf writes a warning where the answer's warnings go, if anywhere.
func (f *answerFlags) warnf(format string, args ...any) {
	if f.warn != nil {
		_, _ = fmt.Fprintf(f.warn, format, args...)
	}
}

// applyModelConfig files m as the model config of every variant of st whose
// recorded engine parameters hold none: a recording made before the config
// was read, or without one. A variant whose recording holds a config keeps
// it, and one that names other classes than m is warned of. A variant with
// no engine parameters is left so: there is no engine to describe.
func applyModelConfig(st *learnreplay.State, m capacity.ModelConfig, warn io.Writer) {
	if st == nil || m.Source == "" {
		return
	}
	for i := range st.Variants {
		v := &st.Variants[i]
		if v.Params == nil {
			continue
		}
		if have := v.Params.ModelConfig; have.Source != "" {
			if warn != nil && (have.Attention != m.Attention || have.NumExperts != m.NumExperts) {
				_, _ = fmt.Fprintf(warn, "capacity-planner: warning: variant %s: the recording's model config (%s: %s) "+
					"differs from %s (%s); the recording's is kept\n", v.Variant, have.Source, have, m.Source, m)
			}
			continue
		}
		p := *v.Params
		p.ModelConfig = m
		v.Params = &p
	}
}
