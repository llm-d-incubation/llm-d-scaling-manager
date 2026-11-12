package whatifsource

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	promsource "github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
)

// The pages that list, for operators, every series a capture reads: the
// reference table, and the guide's manual recording, which saves exactly
// those series. A query that starts reading a new series, or stops reading
// one, fails here until both say so.
const (
	metricsDoc   = "../../docs/reference/capacity-planner-metrics.md"
	recordingYML = "../../docs/guides/record-for-capacity-planner/guide.yaml"
)

var (
	// seriesName matches a series name of the families a capture reads.
	seriesName = regexp.MustCompile(`\b(?:(?:vllm|sglang):[A-Za-z0-9_]+|(?:llm_d_epp|inference_extension|kube)_[A-Za-z0-9_]+)`)
	// selectorName matches any name a label matcher follows: every series
	// selector with matchers. A template that reads a family the pattern
	// above does not know fails the test, rather than going unlisted.
	selectorName = regexp.MustCompile(`([A-Za-z_:][A-Za-z0-9_:]*)\{`)
	// docRow is a row of the reference table: its first cell is the name.
	docRow = regexp.MustCompile("(?m)^\\| `([^`]+)` \\|")
)

// capturedSeries is every series name the capture's queries read, with
// the queries that read it.
func capturedSeries(t *testing.T) map[string][]string {
	t.Helper()
	src := promsource.NewPrometheusSource(context.Background(), nil, promsource.PrometheusSourceConfig{DefaultTTL: time.Second})
	defer src.Close()
	registry := source.NewSourceRegistry()
	require.NoError(t, registry.Register("prometheus", src))
	registerQueries(registry)

	templates := map[string]string{}
	for _, name := range src.QueryList().List() {
		templates[name] = src.QueryList().Get(name).Template
	}
	ksm := reflect.ValueOf(KSMQueries("ns"))
	for i := range ksm.NumField() {
		templates["ksm/"+ksm.Type().Field(i).Name] = ksm.Field(i).String()
	}

	read := map[string][]string{}
	for query, tmpl := range templates {
		for _, m := range selectorName.FindAllStringSubmatch(tmpl, -1) {
			require.Truef(t, seriesName.MatchString(m[1]) && seriesName.FindString(m[1]) == m[1],
				"query %q selects %q, a family this test does not know: add it to seriesName and to the reference table", query, m[1])
		}
		for _, name := range seriesName.FindAllString(tmpl, -1) {
			read[name] = append(read[name], query)
		}
	}
	// An empty scan would pass every comparison below.
	require.Greater(t, len(read), 50, "the scan found only %d series names", len(read))
	return read
}

func TestCapacityPlannerMetricsDocListsEveryCapturedSeries(t *testing.T) {
	read := capturedSeries(t)

	body, err := os.ReadFile(metricsDoc)
	require.NoError(t, err)
	listed := map[string]bool{}
	for _, m := range docRow.FindAllStringSubmatch(string(body), -1) {
		require.Falsef(t, listed[m[1]], "%s lists %q twice", metricsDoc, m[1])
		listed[m[1]] = true
	}

	var missing, extra []string
	for name, queries := range read {
		if !listed[name] {
			sort.Strings(queries)
			missing = append(missing, name+" (read by "+queries[0]+")")
		}
	}
	for name := range listed {
		if _, ok := read[name]; !ok {
			extra = append(extra, name)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	require.Emptyf(t, missing, "a capture reads series %s does not list", metricsDoc)
	require.Emptyf(t, extra, "%s lists series no capture reads", metricsDoc)
}

func TestManualRecordingSavesEveryCapturedSeries(t *testing.T) {
	read := capturedSeries(t)

	body, err := os.ReadFile(recordingYML)
	require.NoError(t, err)
	saved := map[string]bool{}
	for _, name := range seriesName.FindAllString(string(body), -1) {
		saved[name] = true
	}

	var missing, extra []string
	for name := range read {
		if !saved[name] {
			missing = append(missing, name)
		}
	}
	for name := range saved {
		if _, ok := read[name]; !ok {
			extra = append(extra, name)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	require.Emptyf(t, missing, "the manual recording in %s does not save series a capture reads", recordingYML)
	require.Emptyf(t, extra, "the manual recording in %s saves series no capture reads", recordingYML)
}
