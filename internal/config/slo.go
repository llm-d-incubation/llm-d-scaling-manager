package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// sloAnalyzerName is the SLO analyzer's registered name (domain.SLOAnalyzerName),
// spelled here because internal/config is a lower layer than the domain's
// analyzer constants are read from -- the same arrangement as
// throughputAnalyzerName.
const sloAnalyzerName = "slo"

// DefaultTTFTPercentile is the percentile a TTFT target is judged at when the
// policy names none. 90 matches llm-d's SLO-aware recipe.
const DefaultTTFTPercentile = 90

// SLOPolicy is a tier's latency targets. The two objectives are different
// statistics, and that is not a configuration choice:
//
//   - TPOT is a MEAN, the average time per output token. It is the quantity the
//     inter-token-latency line ITL(k) = A*k + B is fitted on, so a TPOT target
//     inverts that line directly.
//   - TTFT is a PERCENTILE. It is dominated by queueing and has a long tail, so a
//     mean says little about it.
//
// See docs/proposals/slo-tiers.md.
type SLOPolicy struct {
	TPOT *SLOObjective `yaml:"tpot,omitempty"`
	TTFT *SLOObjective `yaml:"ttft,omitempty"`
	// FlowControl declares whether the EPP's flowControl feature gate is on,
	// as deployed. Empty is unset: the state is inferred from the router's
	// flow-control series, and an inferred off is flagged, because a scrape
	// gap looks the same.
	FlowControl SLOFlowControl `yaml:"flowControl,omitempty" learnrec:"omitzero"`
}

// SLOFlowControl is the EPP's flowControl feature gate as a tier declares it.
type SLOFlowControl string

const (
	// SLOFlowControlEnabled declares the gate on.
	SLOFlowControlEnabled SLOFlowControl = "enabled"
	// SLOFlowControlDisabled declares the gate off.
	SLOFlowControlDisabled SLOFlowControl = "disabled"
)

// SLOObjective is one latency target.
type SLOObjective struct {
	// Target is the latency to meet, as a Go duration ("100ms", "3s").
	Target SLODuration `yaml:"target"`
	// Percentile is the share of requests, in percent, that must meet Target.
	// TTFT only: zero means DefaultTTFTPercentile. A TPOT objective is a mean
	// and must leave it unset.
	Percentile SLOPercentile `yaml:"percentile,omitempty"`
}

// SLOPercentile is a percentile in whole percent. A fraction is refused: the
// int it decodes into would truncate 0.95 to 0, which is the default p90, and
// 99.9 to 99, each judged without a word.
type SLOPercentile int

// UnmarshalYAML reads a whole number of percent.
func (p *SLOPercentile) UnmarshalYAML(node *yaml.Node) error {
	var n int
	if node.Tag != "!!int" || node.Decode(&n) != nil {
		return fmt.Errorf("percentile %q must be a whole number of percent from 50 to 99 (95, not 0.95)", node.Value)
	}
	*p = SLOPercentile(n)
	return nil
}

// SLODuration is a time.Duration read from a Go duration string. A bare number
// is refused rather than read as nanoseconds.
type SLODuration struct {
	time.Duration
}

// UnmarshalYAML parses a Go duration string.
func (d *SLODuration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("target must be a duration string such as \"100ms\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("target %q is not a duration (use a unit, e.g. \"100ms\" or \"3s\"): %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// Empty reports whether the policy sets no objective at all.
func (s *SLOPolicy) Empty() bool {
	return s == nil || (s.TPOT == nil && s.TTFT == nil)
}

// TTFTPercentile is the TTFT objective's percentile with its default applied,
// or 0 when there is no TTFT objective.
func (s *SLOPolicy) TTFTPercentile() int {
	if s == nil || s.TTFT == nil {
		return 0
	}
	if s.TTFT.Percentile == 0 {
		return DefaultTTFTPercentile
	}
	return int(s.TTFT.Percentile)
}

// Validate checks every objective that is set. A malformed SLO is an error and,
// like every ScalingPolicy error, drops the whole entry it is on.
func (s *SLOPolicy) Validate() error {
	if s == nil {
		return nil
	}
	if s.TPOT != nil {
		if s.TPOT.Target.Duration <= 0 {
			return errors.New("tpot.target must be a positive duration")
		}
		if s.TPOT.Percentile != 0 {
			return errors.New("tpot is a mean: tpot.percentile is not supported")
		}
	}
	if s.TTFT != nil {
		if s.TTFT.Target.Duration <= 0 {
			return errors.New("ttft.target must be a positive duration")
		}
		if p := s.TTFT.Percentile; p != 0 && (p < 50 || p > 99) {
			return fmt.Errorf("ttft.percentile must be between 50 and 99, got %d", p)
		}
	}
	switch s.FlowControl {
	case "", SLOFlowControlEnabled, SLOFlowControlDisabled:
	default:
		return fmt.Errorf("flowControl must be %q or %q, got %q", SLOFlowControlEnabled, SLOFlowControlDisabled, s.FlowControl)
	}
	return nil
}

// PricesTTFT reports whether the policy runs the TTFT pricing: a TTFT target,
// and the SLO analyzer that reads it. A target with the analyzer unlisted is
// read by nothing.
func (c ScalingPolicy) PricesTTFT() bool {
	return c.SLOAnalyzerListed() && c.SLO != nil && c.SLO.TTFT != nil
}

// SLOAnalyzerListed reports whether the policy runs the SLO analyzer.
func (c ScalingPolicy) SLOAnalyzerListed() bool {
	return c.AnalyzerEnabled(sloAnalyzerName)
}

// SLOSummary renders the policy's SLO for a log line: the targets, and whether
// the analyzer that reads them is listed. "off" when neither is set.
func (c ScalingPolicy) SLOSummary() string {
	listed := c.SLOAnalyzerListed()
	if c.SLO.Empty() && !listed {
		return "off"
	}
	var parts []string
	if c.SLO != nil && c.SLO.TPOT != nil {
		parts = append(parts, "tpot mean<="+c.SLO.TPOT.Target.String())
	}
	if c.SLO != nil && c.SLO.TTFT != nil {
		parts = append(parts, fmt.Sprintf("ttft p%d<=%s", c.SLO.TTFTPercentile(), c.SLO.TTFT.Target))
	}
	if len(parts) == 0 {
		parts = append(parts, "no targets")
	}
	analyzer := "analyzer not listed"
	if listed {
		analyzer = "observe"
	}
	return strings.Join(parts, ", ") + " (" + analyzer + ")"
}

// SLOMisconfiguration names a half-configured SLO -- targets the analyzer will
// never read, or the analyzer listed with nothing to aim at -- or returns "".
// It is a warning and not a Validate error, because an error would drop the
// whole policy entry and with it every threshold the operator did set.
func (c ScalingPolicy) SLOMisconfiguration() string {
	listed := c.SLOAnalyzerListed()
	switch {
	case !c.SLO.Empty() && !listed:
		return "slo targets are set but the slo analyzer is not listed under analyzers:, so nothing reads them"
	case c.SLO.Empty() && listed:
		return "the slo analyzer is listed but the policy sets no slo targets, so it has nothing to aim at"
	}
	return ""
}
