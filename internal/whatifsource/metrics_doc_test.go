package whatifsource

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

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
	// docRow is a row of the reference table: its first cell is the name.
	docRow = regexp.MustCompile("(?m)^\\| `([^`]+)` \\|")
	// metricsVar is the METRICS="..." list in the guide's fetch step.
	metricsVar = regexp.MustCompile(`(?s)METRICS="([^"]*)"`)

	// What selectedNames strips before reading the names left: strings
	// (label values, the {{.namespace}} placeholder), matcher bodies,
	// range and subquery brackets, and the label lists of grouping and
	// vector matching.
	promString   = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	promMatchers = regexp.MustCompile(`\{[^{}]*\}`)
	promRange    = regexp.MustCompile(`\[[^\]]*\]`)
	promLabels   = regexp.MustCompile(`\b(?:by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	promIdent    = regexp.MustCompile(`[A-Za-z_:][A-Za-z0-9_:]*`)
	// promKeywords are the PromQL words that are neither a function call
	// nor a series.
	promKeywords = map[string]bool{
		"and": true, "or": true, "unless": true, "bool": true, "offset": true,
		"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
		"inf": true, "nan": true, "Inf": true, "NaN": true,
	}
)

// selectedNames is every series a PromQL expression selects, with or
// without matchers: every identifier left once strings, matchers, ranges
// and label lists are gone, that is neither a function call nor a keyword.
func selectedNames(expr string) []string {
	expr = promString.ReplaceAllString(expr, `""`)
	for promMatchers.MatchString(expr) {
		expr = promMatchers.ReplaceAllString(expr, " ")
	}
	expr = promRange.ReplaceAllString(expr, " ")
	expr = promLabels.ReplaceAllString(expr, " ")
	var names []string
	for _, loc := range promIdent.FindAllStringIndex(expr, -1) {
		name := expr[loc[0]:loc[1]]
		// The unit of a duration or the exponent of a number (5m, 1e3).
		if loc[0] > 0 && strings.ContainsRune("0123456789.", rune(expr[loc[0]-1])) {
			continue
		}
		if promKeywords[name] || strings.HasPrefix(strings.TrimLeft(expr[loc[1]:], " "), "(") {
			continue
		}
		names = append(names, name)
	}
	return names
}

func TestSelectedNames(t *testing.T) {
	require.Equal(t, []string{"a_total", "b", "c:d", "kube_node_labels"}, selectedNames(
		`sum by (pod, le) (rate(a_total{namespace="{{.namespace}}",x=~"y|z"}[5m])) / on (pod) group_left(node) `+
			`(b or c:d) > 0 and kube_node_labels offset 5m`))
}

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
		for _, name := range selectedNames(tmpl) {
			require.Truef(t, seriesName.FindString(name) == name,
				"query %q selects %q, a family this test does not know: add it to seriesName and to the reference table", query, name)
			read[name] = append(read[name], query)
		}
	}
	// An empty scan would pass every comparison below.
	require.Greater(t, len(read), 50, "the scan found only %d series names", len(read))
	return read
}

// compare fails when have and read differ, naming what each lacks.
func compare(t *testing.T, where string, read map[string][]string, have map[string]bool) {
	t.Helper()
	var missing, extra []string
	for name, queries := range read {
		if !have[name] {
			sort.Strings(queries)
			missing = append(missing, name+" (read by "+queries[0]+")")
		}
	}
	for name := range have {
		if _, ok := read[name]; !ok {
			extra = append(extra, name)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	require.Emptyf(t, missing, "a capture reads series %s does not list", where)
	require.Emptyf(t, extra, "%s lists series no capture reads", where)
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
	compare(t, metricsDoc, read, listed)
}

func TestManualRecordingSavesEveryCapturedSeries(t *testing.T) {
	read := capturedSeries(t)

	body, err := os.ReadFile(recordingYML)
	require.NoError(t, err)
	var guide struct {
		Manual struct {
			Fetch struct {
				Run string `yaml:"run"`
			} `yaml:"fetch"`
		} `yaml:"manual"`
	}
	require.NoError(t, yaml.Unmarshal(body, &guide))
	m := metricsVar.FindStringSubmatch(guide.Manual.Fetch.Run)
	require.NotNilf(t, m, "%s: manual.fetch has no METRICS=\"...\" list", recordingYML)
	saved := map[string]bool{}
	for _, name := range strings.Fields(m[1]) {
		require.Truef(t, seriesName.FindString(name) == name, "METRICS in %s holds %q, not a series name", recordingYML, name)
		require.Falsef(t, saved[name], "METRICS in %s lists %q twice", recordingYML, name)
		saved[name] = true
	}
	compare(t, "manual.fetch's METRICS in "+recordingYML, read, saved)
}
