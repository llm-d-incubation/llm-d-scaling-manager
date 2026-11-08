package steadystate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// TestModeLabelForAnalyzer ensures the "Optimization completed successfully" log
// entry reports the analyzer mode that was actually selected, rather than always
// logging "saturation-only" regardless of which path ran.
func TestModeLabelForAnalyzer(t *testing.T) {
	tests := []struct {
		name         string
		analyzerName string
		wantMode     string
	}{
		{"named saturation analyzer", domain.SaturationAnalyzerName, domain.SaturationAnalyzerName},
		{"unset analyzer name falls back to saturation-only", "", "saturation-only"},
		{"unrecognized analyzer name falls back to saturation-only", "not-a-real-analyzer", "saturation-only"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantMode, modeLabelForAnalyzer(tt.analyzerName))
		})
	}
}
