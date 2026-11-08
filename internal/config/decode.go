package config

import (
	"bytes"
	"errors"
	"io"
	"regexp"

	"gopkg.in/yaml.v3"
)

// DecodeScalingPolicy decodes one scaling-policy ConfigMap entry. An error is a
// real decode failure (malformed YAML, a value of the wrong type), and the
// caller rejects the entry. A key ScalingPolicy does not define is NOT an error
// here: the entry applies with that key ignored, and UnknownScalingPolicyKeys
// names it so the caller can report it.
//
// Unknown keys are reported rather than rejected on purpose. Rejecting them
// would turn a stale or misspelled key into a lost entry, and a lost "default"
// is a lost GPU bound: limiters are the safety boundary, so a ConfigMap that
// carried one before an upgrade must keep carrying it after.
//
// An empty entry decodes to the zero ScalingPolicy.
func DecodeScalingPolicy(entry string) (ScalingPolicy, error) {
	var p ScalingPolicy
	if err := yaml.Unmarshal([]byte(entry), &p); err != nil {
		return ScalingPolicy{}, err
	}
	return p, nil
}

// unknownFieldMessage matches yaml.v3's KnownFields error for one key.
var unknownFieldMessage = regexp.MustCompile(`^line (\d+): field (\S+) not found in type `)

// UnknownScalingPolicyKeys returns the keys of an entry that ScalingPolicy does
// not define, at any depth, each as "<key> (line N)". These are the authoring
// mistakes a lenient decode drops without a word: `scaleUpThreshhold`,
// `scale_to_zero`, and `modelID:` in an override body (the key is `model_id`),
// which leaves the override an unselected tier.
//
// defaultOutputTokens is not reported: it is documented as ignored here,
// because it is set on the ScaledObject trigger (see
// ScalingPolicy.DefaultOutputTokens).
//
// Call it on an entry DecodeScalingPolicy accepted; for one it rejects, the
// result is nil.
func UnknownScalingPolicyKeys(entry string) []string {
	var e struct {
		ScalingPolicy `yaml:",inline"`
		// IgnoredDefaultOutputTokens absorbs the one documented, ignored key.
		IgnoredDefaultOutputTokens any `yaml:"defaultOutputTokens,omitempty"`
	}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(entry)))
	dec.KnownFields(true)
	err := dec.Decode(&e)
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return nil
	}
	var unknown []string
	for _, msg := range te.Errors {
		if m := unknownFieldMessage.FindStringSubmatch(msg); m != nil {
			unknown = append(unknown, m[2]+" (line "+m[1]+")")
		}
	}
	return unknown
}
