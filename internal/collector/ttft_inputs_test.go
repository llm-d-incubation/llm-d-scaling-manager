package collector

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The TTFT model's inputs (analyzer-learner.md, 2.9), through the real
// Prometheus parse path: each is read when present, and a missing series, a
// NaN, a failed query or another model's series leaves it absent, never 0.

// ttftBucketSamples is one histogram series per bound for the test pod, the
// model named model.
func ttftBucketSamples(modelName string, byBound map[string]float64) model.Vector {
	vec := make(model.Vector, 0, len(byBound))
	for le, v := range byBound {
		vec = append(vec, &model.Sample{
			Metric: model.Metric{
				seriesModelLabel: model.LabelValue(modelName),
				"pod":            presencePod,
				"instance":       "10.0.0.1:8000",
				"le":             model.LabelValue(le),
			},
			Value:     model.SampleValue(v),
			Timestamp: model.TimeFromUnixNano(time.Now().UnixNano()),
		})
	}
	return vec
}

// ttftHistograms names each histogram, its query and how the row carries it.
var ttftHistograms = []struct {
	name  string
	query string
	get   func(domain.ReplicaMetrics) []domain.LatencyBucket
}{
	{"PromptTokensBuckets", registration.QueryPromptTokensBuckets, func(m domain.ReplicaMetrics) []domain.LatencyBucket {
		return asLatency(m.PromptTokensBuckets)
	}},
	{"ComputedTokensBuckets", registration.QueryComputedTokensBuckets, func(m domain.ReplicaMetrics) []domain.LatencyBucket {
		return asLatency(m.ComputedTokensBuckets)
	}},
	{"OutputTokensBuckets", registration.QueryOutputTokensBuckets, func(m domain.ReplicaMetrics) []domain.LatencyBucket {
		return asLatency(m.OutputTokensBuckets)
	}},
	{"TTFTBuckets", registration.QueryTTFTBuckets, func(m domain.ReplicaMetrics) []domain.LatencyBucket { return m.TTFTBuckets }},
	{"QueueTimeBuckets", registration.QueryQueueTimeBuckets, func(m domain.ReplicaMetrics) []domain.LatencyBucket { return m.QueueTimeBuckets }},
	{"TPOTBuckets", registration.QueryTPOTBuckets, func(m domain.ReplicaMetrics) []domain.LatencyBucket { return m.TPOTBuckets }},
}

// asLatency is a length histogram in the latency shape, so one table covers
// both; nil stays nil.
func asLatency(bs []domain.LengthBucket) []domain.LatencyBucket {
	if bs == nil {
		return nil
	}
	out := make([]domain.LatencyBucket, 0, len(bs))
	for _, b := range bs {
		out = append(out, domain.LatencyBucket(b))
	}
	return out
}

func TestTTFTHistogramsThroughPrometheusParse(t *testing.T) {
	inf := math.Inf(1)
	for _, h := range ttftHistograms {
		t.Run(h.name, func(t *testing.T) {
			cases := []struct {
				name   string
				series map[string]model.Value
				failed map[string]bool
				want   []domain.LatencyBucket
			}{
				{"present, sorted by bound",
					map[string]model.Value{h.query: ttftBucketSamples("test-model", map[string]float64{"+Inf": 3, "0.5": 1, "2": 2.5})},
					nil, []domain.LatencyBucket{{UpperBound: 0.5, Rate: 1}, {UpperBound: 2, Rate: 2.5}, {UpperBound: inf, Rate: 3}}},
				{"an idle pod's zero rates are a reading",
					map[string]model.Value{h.query: ttftBucketSamples("test-model", map[string]float64{"1": 0, "+Inf": 0})},
					nil, []domain.LatencyBucket{{UpperBound: 1, Rate: 0}, {UpperBound: inf, Rate: 0}}},
				{"a NaN bucket and a bound that does not parse are dropped",
					map[string]model.Value{h.query: ttftBucketSamples("test-model", map[string]float64{"1": math.NaN(), "fast": 1, "+Inf": 2})},
					nil, []domain.LatencyBucket{{UpperBound: inf, Rate: 2}}},
				{"every bucket NaN is absent",
					map[string]model.Value{h.query: ttftBucketSamples("test-model", map[string]float64{"1": math.NaN(), "+Inf": math.NaN()})},
					nil, nil},
				{"a negative or infinite rate is dropped",
					map[string]model.Value{h.query: ttftBucketSamples("test-model", map[string]float64{"1": -1, "2": math.Inf(1)})},
					nil, nil},
				{"another model's series is not this one's",
					map[string]model.Value{h.query: ttftBucketSamples("other-model", map[string]float64{"+Inf": 3})},
					nil, nil},
				{"absent series", nil, nil, nil},
				{"failed query", nil, map[string]bool{h.query: true}, nil},
			}
			for _, tc := range cases {
				got := h.get(collectParsed(t, tc.series, tc.failed, nil))
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
				}
			}
		})
	}
}

// The three new means keep a read value with their bits, and leave it 0
// with the bit off when absent: the value never carries a reading the bit
// denies.
func TestTTFTMeansThroughPrometheusParse(t *testing.T) {
	means := []struct {
		name  string
		query string
		get   func(domain.ReplicaMetrics) (float64, bool)
	}{
		{"AvgQueueTime", registration.QueryAvgQueueTime, func(m domain.ReplicaMetrics) (float64, bool) { return m.AvgQueueTime, m.AvgQueueTimeKnown }},
		{"AvgPrefillTime", registration.QueryAvgPrefillTime, func(m domain.ReplicaMetrics) (float64, bool) { return m.AvgPrefillTime, m.AvgPrefillTimeKnown }},
		{"AvgKVTransferTime", registration.QueryAvgKVTransferTime, func(m domain.ReplicaMetrics) (float64, bool) {
			return m.AvgKVTransferTime, m.AvgKVTransferTimeKnown
		}},
		// Not a mean, but read under the same rule: a zero is a queue that
		// emptied in the minute, a NaN or an absent series no reading.
		{"QueueMin", registration.QueryQueueMin, func(m domain.ReplicaMetrics) (float64, bool) { return m.QueueMin, m.QueueMinKnown }},
	}
	for _, mean := range means {
		t.Run(mean.name, func(t *testing.T) {
			cases := []struct {
				name      string
				series    map[string]model.Value
				failed    map[string]bool
				want      float64
				wantKnown bool
			}{
				{"present", map[string]model.Value{mean.query: presenceSample(0.25)}, nil, 0.25, true},
				{"zero is no wait", map[string]model.Value{mean.query: presenceSample(0)}, nil, 0, true},
				{"NaN (no request) is absent", map[string]model.Value{mean.query: presenceSample(math.NaN())}, nil, 0, false},
				{"absent", nil, nil, 0, false},
				{"failed", nil, map[string]bool{mean.query: true}, 0, false},
				{"a finite series and then a NaN keep the reading",
					map[string]model.Value{mean.query: rankSamples("8000", 0.25, math.NaN())}, nil, 0.25, true},
			}
			for _, tc := range cases {
				v, known := mean.get(collectParsed(t, tc.series, tc.failed, nil))
				if v != tc.want || known != tc.wantKnown {
					t.Errorf("%s: got %v (known %v), want %v (known %v)", tc.name, v, known, tc.want, tc.wantKnown)
				}
			}
		})
	}
}

// QueueLengthKnown: the queue series the pod was found by. The pod is found
// by its KV series where the queue's is missing, so an absent queue reads
// unknown rather than as an empty queue.
func TestQueueLengthKnownThroughPrometheusParse(t *testing.T) {
	kv := presenceSample(0.5)
	cases := []struct {
		name  string
		queue model.Value
		want  bool
	}{
		{"present", presenceSample(3), true},
		{"an empty queue", presenceSample(0), true},
		{"NaN", presenceSample(math.NaN()), false},
		{"+Inf", presenceSample(math.Inf(1)), false},
		{"negative", presenceSample(-1), false},
		{"absent", model.Vector{}, false},
	}
	for _, tc := range cases {
		got := collectParsed(t, map[string]model.Value{
			registration.QueryKvCacheUsage: kv,
			registration.QueryQueueLength:  tc.queue,
		}, nil, nil)
		if got.QueueLengthKnown != tc.want {
			t.Errorf("%s: QueueLengthKnown = %v, want %v", tc.name, got.QueueLengthKnown, tc.want)
		}
		// The value is kept as it always was; only the bit is new.
		if tc.name == "absent" && got.QueueLength != 0 {
			t.Errorf("%s: QueueLength = %d with no series", tc.name, got.QueueLength)
		}
	}
}

// A not-Ready Pod's service time and TTFT are dropped, and their bits with
// them; the TTFT model's new inputs are kept as read, for the row's Ready to
// gate.
func TestTTFTInputsOnANotReadyPod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: presencePod, Namespace: "test-ns"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}).Build()
	got := collectParsed(t, map[string]model.Value{
		registration.QueryAvgServiceTime: presenceSample(0.5),
		registration.QueryAvgTTFT:        presenceSample(0.2),
		registration.QueryAvgQueueTime:   presenceSample(0.1),
		registration.QueryTTFTBuckets:    ttftBucketSamples("test-model", map[string]float64{"+Inf": 1}),
	}, nil, reader)
	if got.Ready {
		t.Fatal("the Pod should read not Ready")
	}
	if got.AvgServiceTimeKnown || got.AvgTTFTKnown || got.AvgServiceTime != 0 || got.AvgTTFT != 0 {
		t.Errorf("a not-Ready pod kept a timing: service %v (known %v), TTFT %v (known %v)",
			got.AvgServiceTime, got.AvgServiceTimeKnown, got.AvgTTFT, got.AvgTTFTKnown)
	}
	if !got.AvgQueueTimeKnown || got.AvgQueueTime != 0.1 || len(got.TTFTBuckets) != 1 {
		t.Errorf("the new inputs were dropped: queue time %v (known %v), TTFT buckets %v",
			got.AvgQueueTime, got.AvgQueueTimeKnown, got.TTFTBuckets)
	}
}

// A service time or TTFT longer than the Pod has run is dropped, and the bit
// with it.
func TestTimingBitsFollowTheUptimeGuard(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	started := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: presencePod, Namespace: "test-ns"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			StartTime:  &started,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}).Build()
	got := collectParsed(t, map[string]model.Value{
		registration.QueryAvgServiceTime: presenceSample(3600),
		registration.QueryAvgTTFT:        presenceSample(3600),
	}, nil, reader)
	if !got.Ready {
		t.Fatal("the Pod should read Ready")
	}
	if got.AvgServiceTimeKnown || got.AvgTTFTKnown {
		t.Errorf("a mean longer than the pod has run kept its bit: service %v, TTFT %v",
			got.AvgServiceTimeKnown, got.AvgTTFTKnown)
	}
}

// A pod's ranks merge: each bound's rate summed, a histogram only when every
// rank has one; a mean weighted by request rate, known only when every rank's
// is.
func TestMergePodInstances_TTFTInputs(t *testing.T) {
	rank := func(rate, queueTime float64, known bool, buckets []domain.LatencyBucket) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: "p", ModelID: "m", RequestRate: rate,
			AvgQueueTime: queueTime, AvgQueueTimeKnown: known, TTFTBuckets: buckets,
			PromptTokensBuckets:   []domain.LengthBucket{{UpperBound: 100, Rate: rate}},
			ComputedTokensBuckets: []domain.LengthBucket{{UpperBound: 200, Rate: rate}}}
	}
	a := []domain.LatencyBucket{{UpperBound: 1, Rate: 1}, {UpperBound: math.Inf(1), Rate: 2}}
	b := []domain.LatencyBucket{{UpperBound: 0.5, Rate: 1}, {UpperBound: 1, Rate: 3}, {UpperBound: math.Inf(1), Rate: 4}}

	got := collapseToPods([]domain.ReplicaMetrics{rank(1, 0.1, true, a), rank(3, 0.3, true, b)})[0]
	wantBuckets := []domain.LatencyBucket{{UpperBound: 0.5, Rate: 1}, {UpperBound: 1, Rate: 4}, {UpperBound: math.Inf(1), Rate: 6}}
	if !reflect.DeepEqual(got.TTFTBuckets, wantBuckets) {
		t.Errorf("TTFTBuckets = %v, want %v", got.TTFTBuckets, wantBuckets)
	}
	if want := []domain.LengthBucket{{UpperBound: 100, Rate: 4}}; !reflect.DeepEqual(got.PromptTokensBuckets, want) {
		t.Errorf("PromptTokensBuckets = %v, want %v", got.PromptTokensBuckets, want)
	}
	if want := []domain.LengthBucket{{UpperBound: 200, Rate: 4}}; !reflect.DeepEqual(got.ComputedTokensBuckets, want) {
		t.Errorf("ComputedTokensBuckets = %v, want %v", got.ComputedTokensBuckets, want)
	}
	noHistogram := rank(3, 0.3, true, b)
	noHistogram.ComputedTokensBuckets = nil
	if got := collapseToPods([]domain.ReplicaMetrics{rank(1, 0.1, true, a), noHistogram})[0]; got.ComputedTokensBuckets != nil {
		t.Errorf("a rank without a computed-token histogram left the pod one: %v", got.ComputedTokensBuckets)
	}
	// (0.1 x 1 + 0.3 x 3) / 4
	if !got.AvgQueueTimeKnown || math.Abs(got.AvgQueueTime-0.25) > 1e-12 {
		t.Errorf("AvgQueueTime = %v (known %v), want 0.25 known", got.AvgQueueTime, got.AvgQueueTimeKnown)
	}

	got = collapseToPods([]domain.ReplicaMetrics{rank(1, 0.1, true, a), rank(3, 0, false, nil)})[0]
	if got.TTFTBuckets != nil {
		t.Errorf("a rank without a histogram left the pod one: %v", got.TTFTBuckets)
	}
	if got.AvgQueueTimeKnown || got.AvgQueueTime != 0 {
		t.Errorf("a rank without a mean left the pod one: %v (known %v)", got.AvgQueueTime, got.AvgQueueTimeKnown)
	}

	// With each rank's queue-time histogram, its +Inf bucket (the requests
	// that left the queue) weights the mean, not the request rate:
	// (0.1 x 6 + 0.3 x 2) / 8.
	withCount := func(rate, queueTime, queued float64) domain.ReplicaMetrics {
		m := rank(rate, queueTime, true, a)
		m.QueueTimeBuckets = []domain.LatencyBucket{{UpperBound: 1, Rate: queued / 2}, {UpperBound: math.Inf(1), Rate: queued}}
		return m
	}
	got = collapseToPods([]domain.ReplicaMetrics{withCount(1, 0.1, 6), withCount(3, 0.3, 2)})[0]
	if !got.AvgQueueTimeKnown || math.Abs(got.AvgQueueTime-0.15) > 1e-12 {
		t.Errorf("AvgQueueTime by queue count = %v (known %v), want 0.15 known", got.AvgQueueTime, got.AvgQueueTimeKnown)
	}
	// One rank without the histogram: the whole pod falls back to request
	// rate, never a mix of the two weights.
	got = collapseToPods([]domain.ReplicaMetrics{withCount(1, 0.1, 6), rank(3, 0.3, true, a)})[0]
	if !got.AvgQueueTimeKnown || math.Abs(got.AvgQueueTime-0.25) > 1e-12 {
		t.Errorf("AvgQueueTime falling back = %v (known %v), want 0.25 known", got.AvgQueueTime, got.AvgQueueTimeKnown)
	}
}

// QueueMin merges by sum, each rank's least summed (a floor under the pod's
// own least), and is known only when every rank read one.
func TestMergePodInstances_QueueMin(t *testing.T) {
	rank := func(v float64, known bool) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: "p", ModelID: "m", QueueMin: v, QueueMinKnown: known}
	}
	got := collapseToPods([]domain.ReplicaMetrics{rank(2, true), rank(0, true), rank(1, true)})[0]
	if !got.QueueMinKnown || got.QueueMin != 3 {
		t.Errorf("QueueMin = %v (known %v), want 3 known", got.QueueMin, got.QueueMinKnown)
	}
	got = collapseToPods([]domain.ReplicaMetrics{rank(2, true), rank(0, false)})[0]
	if got.QueueMinKnown || got.QueueMin != 0 {
		t.Errorf("a rank without one left the pod one: %v (known %v)", got.QueueMin, got.QueueMinKnown)
	}
}
