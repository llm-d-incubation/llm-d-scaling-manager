package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestUnknownKeysAreNamedNotFatal: a key the schema does not define must not
// cost the entry (that would drop a "default" and its GPU bound), but it must
// be named, with its line.
func TestUnknownKeysAreNamedNotFatal(t *testing.T) {
	for name, tc := range map[string]struct{ entry, want string }{
		"misspelled threshold":       {"kvCacheThreshold: 0.8\nscaleUpThreshhold: 0.9\n", "scaleUpThreshhold (line 2)"},
		"snake-case block":           {"kvCacheThreshold: 0.8\nscale_to_zero:\n  enabled: true\n", "scale_to_zero (line 2)"},
		"trigger spelling of the id": {"modelID: meta/llama\nnamespace: team-a\n", "modelID (line 1)"},
		"unknown nested key":         {"scaleToZero:\n  enabeld: true\n", "enabeld (line 2)"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeScalingPolicy(tc.entry); err != nil {
				t.Errorf("an unknown key rejected the entry: %v", err)
			}
			if got := UnknownScalingPolicyKeys(tc.entry); len(got) != 1 || got[0] != tc.want {
				t.Errorf("unknown keys = %v, want [%s]", got, tc.want)
			}
		})
	}
}

func TestWrongTypeIsAnError(t *testing.T) {
	if _, err := DecodeScalingPolicy("kvCacheThreshold: high\n"); err == nil {
		t.Error("a value of the wrong type was accepted")
	}
}

func TestDefaultOutputTokensIsNotReported(t *testing.T) {
	entry := "kvCacheThreshold: 0.8\ndefaultOutputTokens: 6000\n"
	p, err := DecodeScalingPolicy(entry)
	if err != nil {
		t.Fatalf("the documented, ignored key rejected the entry: %v", err)
	}
	if p.DefaultOutputTokens != 0 || p.KvCacheThreshold != 0.8 {
		t.Errorf("decoded %+v", p)
	}
	if got := UnknownScalingPolicyKeys(entry); got != nil {
		t.Errorf("the documented, ignored key was reported as unknown: %v", got)
	}
}

func TestDecodeScalingPolicyAcceptsKnownAndEmpty(t *testing.T) {
	p, err := DecodeScalingPolicy("model_id: meta/llama\nnamespace: team-a\nscaleUpThreshold: 0.9\n")
	if err != nil {
		t.Fatal(err)
	}
	if p.ModelID != "meta/llama" || p.ScaleUpThreshold != 0.9 {
		t.Errorf("decoded %+v", p)
	}
	if _, err := DecodeScalingPolicy(""); err != nil {
		t.Errorf("an empty entry: %v", err)
	}
}

// TestShippedScalingPolicyConfigMapsDecodeStrictly: every scaling-policy
// ConfigMap the repository ships or applies must decode with no unknown key, or
// it would warn on every load.
func TestShippedScalingPolicyConfigMapsDecodeStrictly(t *testing.T) {
	files := []string{
		"../../config/base/manager/scaling-policy-configmap.yaml",
		"../../deploy/configmap-scaling-policy.yaml",
		"../../config/samples/analyzers/external-analyzers.yaml",
		"../../hack/benchmark/scenarios/scaling_manager_threshold/saturation_v2_config.yaml",
	}
	entries := 0
	for _, path := range files {
		before := entries
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		for {
			var doc struct {
				Kind     string                `yaml:"kind"`
				Metadata struct{ Name string } `yaml:"metadata"`
				Data     map[string]string     `yaml:"data"`
			}
			if err := dec.Decode(&doc); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Fatalf("%s: %v", path, err)
			}
			if doc.Kind != "ConfigMap" || !strings.Contains(doc.Metadata.Name, "scaling") {
				continue
			}
			for key, entry := range doc.Data {
				entries++
				if _, err := DecodeScalingPolicy(entry); err != nil {
					t.Errorf("%s entry %q: %v", path, key, err)
				}
				if unknown := UnknownScalingPolicyKeys(entry); unknown != nil {
					t.Errorf("%s entry %q: keys the schema does not define: %v", path, key, unknown)
				}
			}
		}
		if entries == before {
			t.Errorf("%s: no scaling-policy entry found; the filter is wrong or the file moved", path)
		}
	}
	t.Logf("%d entries decoded, none with an unknown key", entries)
}
