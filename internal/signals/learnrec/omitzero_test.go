package learnrec

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

type withOmitted struct {
	Kept    float64
	Flag    bool                  `learnrec:"omitzero"`
	Rate    float64               `learnrec:"omitzero"`
	List    []float64             `learnrec:"omitzero"`
	Ptr     *domain.LatencyBucket `learnrec:"omitzero"`
	JSONTag float64               `json:",omitempty"`
}

// A field tagged omitzero is left out while zero and written otherwise; the
// zero it reads back as is what was left out, so the bytes do not change.
// Nil and empty stay apart, a NaN is not zero, and a json tag still hides
// nothing.
func TestCodec_OmitZero(t *testing.T) {
	b, err := Marshal(withOmitted{})
	require.NoError(t, err)
	assert.Equal(t, `{"Kept":0,"JSONTag":0}`, string(b))

	for _, tc := range []struct {
		in   withOmitted
		want string
	}{
		{withOmitted{Flag: true}, `{"Kept":0,"Flag":true,"JSONTag":0}`},
		{withOmitted{Rate: math.NaN()}, `{"Kept":0,"Rate":"NaN","JSONTag":0}`},
		{withOmitted{List: []float64{}}, `{"Kept":0,"List":[],"JSONTag":0}`},
		{withOmitted{List: []float64{1.5}, Ptr: &domain.LatencyBucket{}}, `{"Kept":0,"List":[1.5],"Ptr":{"UpperBound":0,"Rate":0},"JSONTag":0}`},
	} {
		b, err := Marshal(tc.in)
		require.NoError(t, err)
		assert.Equal(t, tc.want, string(b))
		var out withOmitted
		require.NoError(t, Unmarshal(b, &out))
		again, err := Marshal(out)
		require.NoError(t, err)
		assert.Equal(t, string(b), string(again), "Marshal of what Unmarshal read is the same bytes")
	}
}

// The fields R1 added for the TTFT model (analyzer-learner.md, 2.9) are each
// tagged omitzero, and the fields that were there before R1 are not: a
// recording made before R1 holds exactly the untagged fields, so it reads
// back and is written again byte for byte, and the new fields stay absent
// from it. A field added to one of these types later is listed here.
func TestCodec_R1FieldsAreLeftOutWhileZero(t *testing.T) {
	r1 := map[reflect.Type][]string{
		reflect.TypeFor[domain.ReplicaMetrics](): {"QueueLengthKnown", "AvgServiceTimeKnown", "AvgTTFTKnown",
			"PromptTokensBuckets", "ComputedTokensBuckets", "OutputTokensBuckets", "TTFTBuckets", "AvgQueueTime", "AvgQueueTimeKnown",
			"QueueTimeBuckets", "AvgPrefillTime", "AvgPrefillTimeKnown", "AvgKVTransferTime",
			"AvgKVTransferTimeKnown", "TPOTBuckets", "QueueMin", "QueueMinKnown"},
		reflect.TypeFor[domain.RouterLatency](): {"TTFTSumRate", "TTFTCountRate", "TPOTBuckets", "FlowControlWaitBuckets"},
		reflect.TypeFor[Input]():                {"RouterLatency", "FlowControlPools"},
	}
	for typ, added := range r1 {
		var tagged []string
		for i := range typ.NumField() {
			if typ.Field(i).Tag.Get("learnrec") == OmitZeroTag {
				tagged = append(tagged, typ.Field(i).Name)
			}
		}
		assert.ElementsMatch(t, added, tagged, "%s: the omitzero fields are R1's", typ)
	}
	// Every field of a pool is new, so all are left out while zero.
	pool := reflect.TypeFor[domain.FlowControlPool]()
	for i := range pool.NumField() {
		assert.Equal(t, OmitZeroTag, pool.Field(i).Tag.Get("learnrec"), "FlowControlPool.%s", pool.Field(i).Name)
	}

	// A row as a pre-R1 recording holds it: every old field set, every R1
	// field zero. None of R1's names is written, and the bytes read back.
	row := domain.ReplicaMetrics{PodName: "p", QueueLength: 3, AvgTTFT: 0.2, AvgServiceTime: 0.5,
		KvCacheUsageKnown: true, AvgITL: 0.02, AvgITLKnown: true, Ready: true}
	in := Input{ModelID: "m", Namespace: "ns", ReplicaMetrics: []domain.ReplicaMetrics{row},
		SchedulerQueue: &domain.SchedulerQueueMetrics{QueueSize: 1}, ArrivalRate: 2}
	b, err := Marshal(in)
	require.NoError(t, err)
	for _, names := range r1 {
		for _, name := range names {
			assert.NotContains(t, string(b), `"`+name+`":`, "a zero R1 field was written")
		}
	}
	var out Input
	require.NoError(t, Unmarshal(b, &out))
	again, err := Marshal(out)
	require.NoError(t, err)
	assert.Equal(t, string(b), string(again))

	// Set, each is written, and reads back.
	in.ReplicaMetrics[0].AvgTTFTKnown = true
	in.ReplicaMetrics[0].TTFTBuckets = []domain.LatencyBucket{{UpperBound: math.Inf(1), Rate: 1}}
	in.RouterLatency = &domain.RouterLatency{WindowSeconds: 300,
		TPOTBuckets: []domain.LatencyBucket{{UpperBound: 0.05, Rate: 0.5}}}
	in.FlowControlPools = []domain.FlowControlPool{{WindowSeconds: 300, DecodeSaturation: 0.9, DecodeSaturationKnown: true}}
	b, err = Marshal(in)
	require.NoError(t, err)
	for _, want := range []string{`"AvgTTFTKnown":true`, `"TTFTBuckets":[{"UpperBound":"+Inf","Rate":1}]`,
		`"TPOTBuckets":[{"UpperBound":0.05,"Rate":0.5}]`, `"FlowControlPools":[{"WindowSeconds":300,"DecodeSaturation":0.9,"DecodeSaturationKnown":true}]`} {
		assert.True(t, strings.Contains(string(b), want), "missing %s in %s", want, b)
	}
	out = Input{}
	require.NoError(t, Unmarshal(b, &out))
	assert.Equal(t, in, out)

	// A zero read is not a zero omitted: the bit is written, the value is
	// not, and both read back. A queue that never waited (AvgQueueTime 0,
	// known) stays known, and stays apart from no reading at all.
	in.ReplicaMetrics[0].AvgQueueTime, in.ReplicaMetrics[0].AvgQueueTimeKnown = 0, true
	b, err = Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"AvgQueueTimeKnown":true`)
	assert.NotContains(t, string(b), `"AvgQueueTime":`)
	out = Input{}
	require.NoError(t, Unmarshal(b, &out))
	assert.Equal(t, in, out)
	assert.True(t, out.ReplicaMetrics[0].AvgQueueTimeKnown)
	assert.Zero(t, out.ReplicaMetrics[0].AvgQueueTime)
	again, err = Marshal(out)
	require.NoError(t, err)
	assert.Equal(t, string(b), string(again))
}
