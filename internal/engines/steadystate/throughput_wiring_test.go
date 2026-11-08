package steadystate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/cycleclock"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

// The throughput analyzer cmd/main registers is wired to the engine: the
// settle tracker the engine writes, the capacity store, the optimize
// interval, and the saturation analyzer's bucket retention as its slope hold.
func TestEngine_TheThroughputAnalyzerIsWiredToTheEngine(t *testing.T) {
	store := capacity.NewStore()
	sat := saturation.NewSaturationAnalyzer(store)
	cfg := config.NewTestConfig()
	e := &Engine{
		Config:        cfg,
		capacityStore: store,
		settle:        settle.NewTracker(),
		clock:         cycleclock.New(clocktesting.NewFakePassiveClock(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))),
		analyzers:     newAnalyzerRegistry(analyzerEntry{name: domain.SaturationAnalyzerName, analyzer: sat}),
	}
	require.Positive(t, sat.Horizons().BucketRetention, "the case needs a bucket retention to pass on")
	got := e.NewThroughputAnalyzer().StartConfig()
	assert.Equal(t, learnrec.ThroughputStart{
		SlopeHoldFor:         sat.Horizons().BucketRetention,
		OptimizationInterval: cfg.OptimizationInterval(),
		SettleView:           true,
		CapacityStore:        true,
	}, got)
}

// An engine a test built by hand with nothing gives an analyzer with
// nothing, and does not panic.
func TestEngine_ABareEngineGivesABareThroughputAnalyzer(t *testing.T) {
	assert.Equal(t, learnrec.ThroughputStart{}, (&Engine{}).NewThroughputAnalyzer().StartConfig())
}
