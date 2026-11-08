package steadystate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

// The learned-state document as v0.10.1 reads it, frozen, so a test can hold
// the current document to what the previous release decodes: a rollback reads
// the document this build wrote.
//
// Copied from tag v0.10.1, commit 760f76d76e179015a8f98592337a34d9720e1d6e,
// struct declarations only (fields, tags and field order; no methods; the
// embedded RollingAverageSnapshot kept embedded), each name prefixed v0101:
//
//	internal/engines/steadystate/learned_state_store.go         blob d1f8d339c7a0b13d8af29b1d8c0c5c85e4a9c303
//	internal/engines/analyzers/saturation/learned_snapshot.go   blob b8277be9e29d9bfb1f8e0586f056c58ac75307b8
//	internal/engines/analyzers/saturation/forget_variant.go     blob d10d6a82f7cf12d2affba0be9e428226ef70384b
//	internal/signals/capacity/rolling_snapshot.go               blob a3157ef25c846e9807dfdc39f15823bb2202886c
//
// v0.10.2 (de074bb2f04d) has the same four blobs, so this is its decoder too.
// v0.10.1 reads the document with encoding/json's Unmarshal, which ignores a
// field it does not know. Do not edit these: TestV0101Decoder_TagSetIsPinned
// fails on any change.

type v0101LearnedStateDoc struct {
	Version   int                     `json:"version"`
	WrittenAt time.Time               `json:"writtenAt"`
	Writer    v0101LearnedStateWriter `json:"writer"`
	State     v0101LearnedState       `json:"state"`
}

type v0101LearnedStateWriter struct {
	Namespace string `json:"namespace"`
	Identity  string `json:"identity"`
}

type v0101LearnedState struct {
	K2               map[string]v0101RollingAverageSnapshot `json:"k2,omitempty"`
	Throughput       map[string]v0101ThroughputSnapshot     `json:"throughput,omitempty"`
	ITLBaseline      map[string]float64                     `json:"itlBaseline,omitempty"`
	ITLBaselineStash map[string]float64                     `json:"itlBaselineStash,omitempty"`
	ITLSlopes        map[string]v0101SlopeSnapshot          `json:"itlSlopes,omitempty"`
	StartSeconds     map[string]float64                     `json:"startSeconds,omitempty"`
	VariantSeenAt    map[string]time.Time                   `json:"variantSeenAt,omitempty"`
	Accelerators     map[string]v0101AcceleratorSnapshot    `json:"accelerators,omitempty"`
	ITLLines         map[string]v0101LineSnapshot           `json:"itlLines,omitempty"`
	K2BorrowedSince  map[string]time.Time                   `json:"k2BorrowedSince,omitempty"`
	LowerBounds      map[string]v0101LowerBoundSnapshot     `json:"lowerBounds,omitempty"`
	K2Args           map[string]string                      `json:"k2Args,omitempty"`
	ThroughputArgs   map[string]string                      `json:"throughputArgs,omitempty"`
	K2Stash          map[string]v0101RollingAverageSnapshot `json:"k2Stash,omitempty"`
	ThroughputStash  map[string]v0101ThroughputSnapshot     `json:"throughputStash,omitempty"`
	Forgotten        map[string]v0101ForgetMarkSnapshot     `json:"forgotten,omitempty"`
	Config           map[string]string                      `json:"config,omitempty"`
	Args             map[string]string                      `json:"args,omitempty"`
	PriorK2          map[string]v0101PriorK2Snapshot        `json:"priorK2,omitempty"`
}

type v0101RollingAverageSnapshot struct {
	Values      []float64 `json:"values"`
	LastUsed    time.Time `json:"lastUsed"`
	LastWritten time.Time `json:"lastWritten"`
}

type v0101PriorK2Snapshot struct {
	K2          int64     `json:"k2"`
	InstalledAt time.Time `json:"installedAt"`
}

type v0101ThroughputSnapshot struct {
	v0101RollingAverageSnapshot
	SampledAt   time.Time `json:"sampledAt"`
	LastRead    float64   `json:"lastRead"`
	HeldSeconds float64   `json:"heldSeconds,omitempty"`
}

type v0101SlopeSnapshot struct {
	A        float64   `json:"a"`
	FittedAt time.Time `json:"fittedAt"`
	Args     string    `json:"args,omitempty"`
}

type v0101AcceleratorSnapshot struct {
	Name     string    `json:"name"`
	LastUsed time.Time `json:"lastUsed"`
}

type v0101LowerBoundSnapshot struct {
	Armed           bool      `json:"armed,omitempty"`
	Since           time.Time `json:"since,omitzero"`
	Tick            time.Time `json:"tick,omitzero"`
	Seen            time.Time `json:"seen"`
	Donor           string    `json:"donor,omitempty"`
	Own             bool      `json:"own,omitempty"`
	DonorK2         int64     `json:"donorK2,omitempty"`
	Args            string    `json:"args,omitempty"`
	ArgsVersion     int       `json:"argsVersion,omitempty"`
	Offered         int64     `json:"offered,omitempty"`
	Prior           int64     `json:"prior,omitempty"`
	PriorSince      time.Time `json:"priorSince,omitzero"`
	PriorAgeSeconds float64   `json:"priorAgeSeconds,omitempty"`
	PriorUsed       time.Time `json:"priorUsed,omitzero"`
	FigureWritten   time.Time `json:"figureWritten,omitzero"`
	RearmPending    bool      `json:"rearmPending,omitempty"`
}

type v0101LineSnapshot struct {
	A           float64   `json:"a"`
	B           float64   `json:"b"`
	FromVariant string    `json:"fromVariant"`
	LearnedAt   time.Time `json:"learnedAt"`
}

type v0101ForgetMarkSnapshot struct {
	Token string    `json:"token"`
	Seen  time.Time `json:"seen"`
}

// fillNonZeroValue sets every field v reaches to a non-zero value, each one
// distinct where the kind allows: a map and a slice get one entry, a pointer
// a value. A field it cannot fill fails the test, so a kind added to the
// document is noticed here.
func fillNonZeroValue(t *testing.T, v reflect.Value, path string, n *int) {
	t.Helper()
	*n++
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString(fmt.Sprintf("s%d", *n))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(*n))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(*n) + 0.5)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillNonZeroValue(t, p.Elem(), path+"*", n)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZeroValue(t, s.Index(0), path+"[]", n)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillNonZeroValue(t, k, path+"{key}", n)
		e := reflect.New(v.Type().Elem()).Elem()
		fillNonZeroValue(t, e, path+"{}", n)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Date(2026, 10, 1, 9, 0, *n, 0, time.UTC)))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillNonZeroValue(t, v.Field(i), path+"."+v.Type().Field(i).Name, n)
			}
		}
	default:
		t.Fatalf("%s: a %s the filler does not know", path, v.Kind())
	}
}

// assertNoZeroField fails for every field v reaches that is zero: an empty
// map or slice, a nil pointer, a zero scalar or time.
func assertNoZeroField(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	if v.IsZero() {
		t.Errorf("%s came back zero: v0.10.1 does not read it from the current document", path)
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		assertNoZeroField(t, v.Elem(), path)
	case reflect.Slice:
		for i := range v.Len() {
			assertNoZeroField(t, v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			assertNoZeroField(t, it.Value(), fmt.Sprintf("%s{%v}", path, it.Key()))
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			return
		}
		for i := range v.NumField() {
			assertNoZeroField(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	}
}

// assertJSONSubset fails for every key of got, at any depth, that want does
// not hold with the same value: what v0.10.1 read is what the current
// document says.
func assertJSONSubset(t *testing.T, got, want any, path string) {
	t.Helper()
	switch g := got.(type) {
	case map[string]any:
		w, ok := want.(map[string]any)
		if !ok {
			t.Errorf("%s: v0.10.1 read an object, the document holds %T", path, want)
			return
		}
		for k, gv := range g {
			wv, ok := w[k]
			if !ok {
				t.Errorf("%s.%s: v0.10.1 read a field the document does not hold", path, k)
				continue
			}
			assertJSONSubset(t, gv, wv, path+"."+k)
		}
	case []any:
		w, ok := want.([]any)
		if !ok || len(w) != len(g) {
			t.Errorf("%s: v0.10.1 read %v, the document holds %v", path, got, want)
			return
		}
		for i := range g {
			assertJSONSubset(t, g[i], w[i], fmt.Sprintf("%s[%d]", path, i))
		}
	default:
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: v0.10.1 read %v, the document holds %v", path, got, want)
		}
	}
}

// The previous release reads the current document: a state with every field
// set, encoded by the production encoder, decodes into v0.10.1's types with
// every field v0.10.1 knows set, and each one equal to what the document
// holds. The version and writer it checks are in place, so it would restore
// the document rather than reject it.
//
// Negative control: renaming a tag v0.10.1 reads (startSeconds, say) leaves
// that field zero here and fails.
func TestV0101Decoder_ReadsTheCurrentDocument(t *testing.T) {
	var s saturation.LearnedState
	n := 0
	fillNonZeroValue(t, reflect.ValueOf(&s).Elem(), "LearnedState", &n)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	raw, dropped, err := encodeLearnedState(s, nil, now, learnedStateMaxBytes)
	require.NoError(t, err)
	require.Zero(t, dropped, "precondition: the whole state fits")

	var old v0101LearnedStateDoc
	require.NoError(t, json.Unmarshal([]byte(raw), &old), "v0.10.1 cannot decode the current document")
	assert.Equal(t, learnedStateVersion, old.Version)
	assert.Equal(t, 1, old.Version, "v0.10.1 restores version 1 only")
	assert.True(t, old.WrittenAt.Equal(now))
	assertNoZeroField(t, reflect.ValueOf(old), "v0101LearnedStateDoc")

	reencoded, err := json.Marshal(old)
	require.NoError(t, err)
	var got, want any
	require.NoError(t, json.Unmarshal(reencoded, &got))
	require.NoError(t, json.Unmarshal([]byte(raw), &want))
	assertJSONSubset(t, got, want, "doc")
}

// The same with sightings, a field beside State that the filler does not
// reach: v0.10.1 reads the document with every field it knows intact, and a
// settle key does not stop it. v0.10.2's types are v0.10.1's blob for blob,
// and v0.10.0's differ by itlSlopes alone, so this stands for all three:
// each ignores a field it does not know the same way.
func TestV0101Decoder_ReadsADocumentWithSightings(t *testing.T) {
	var s saturation.LearnedState
	n := 0
	fillNonZeroValue(t, reflect.ValueOf(&s).Elem(), "LearnedState", &n)
	require.NotEmpty(t, s.StartCounted, "the filler reached the new field")
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	sightings := map[string][]settle.Sighting{"ns/m": {{PodName: "p0", VariantName: "v", FirstSeen: now,
		ReadySince: now, ReadySeen: now}}}
	raw, dropped, err := encodeLearnedState(s, sightings, now, learnedStateMaxBytes)
	require.NoError(t, err)
	require.Zero(t, dropped)
	var all map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &all))
	require.Contains(t, all, "settle", "the field is written under its name")
	require.Contains(t, all["state"], "startCounted")

	var old v0101LearnedStateDoc
	require.NoError(t, json.Unmarshal([]byte(raw), &old))
	assert.Equal(t, 1, old.Version)
	assertNoZeroField(t, reflect.ValueOf(old), "v0101LearnedStateDoc")
	reencoded, err := json.Marshal(old)
	require.NoError(t, err)
	var got, want any
	require.NoError(t, json.Unmarshal(reencoded, &got))
	require.NoError(t, json.Unmarshal([]byte(raw), &want))
	assertJSONSubset(t, got, want, "doc")
}

// jsonFieldsOf is t's fields by JSON name, an embedded struct's flattened in
// as encoding/json flattens them, each with its tag options.
func jsonFieldsOf(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			for k, v := range jsonFieldsOf(f.Type) {
				out[k] = v
			}
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}
		out[name] = f
	}
	return out
}

// assertKnownTo fails for every object key in doc, at any depth, that the
// frozen type typ has no field for.
func assertKnownTo(t *testing.T, doc any, typ reflect.Type, path string) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch d := doc.(type) {
	case map[string]any:
		switch typ.Kind() {
		case reflect.Map:
			for k, v := range d {
				assertKnownTo(t, v, typ.Elem(), path+"{"+k+"}")
			}
		case reflect.Struct:
			fields := jsonFieldsOf(typ)
			for k, v := range d {
				f, ok := fields[k]
				if !ok {
					t.Errorf("%s.%s: written while empty, and v0.10.1 does not know it; a field added to the document must be omitempty or omitzero", path, k)
					continue
				}
				assertKnownTo(t, v, f.Type, path+"."+k)
			}
		default:
			t.Errorf("%s: an object where v0.10.1 reads a %s", path, typ)
		}
	case []any:
		for i, v := range d {
			assertKnownTo(t, v, typ.Elem(), fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

// The document stays v1 and additive: a field v0.10.1 does not know is left
// out while empty, so the document a rollback reads is the one it wrote. Each
// map v0.10.1 knows holds one zero entry, so a field added to an entry type
// is reached too; the maps it does not know stay empty.
//
// Negative control: a field added to LearnedState, or to an entry type such
// as LowerBoundSnapshot, without omitempty fails here.
func TestV0101Decoder_ANewFieldIsLeftOutWhileEmpty(t *testing.T) {
	known := jsonFieldsOf(reflect.TypeFor[v0101LearnedState]())
	var s saturation.LearnedState
	v := reflect.ValueOf(&s).Elem()
	filled := 0
	for name, f := range jsonFieldsOf(v.Type()) {
		if _, ok := known[name]; !ok {
			continue
		}
		fv := v.FieldByIndex(f.Index)
		require.Equal(t, reflect.Map, fv.Kind(), "LearnedState.%s", f.Name)
		m := reflect.MakeMap(fv.Type())
		m.SetMapIndex(reflect.ValueOf("ns|v|m|H100|1|both|short|q5"), reflect.New(fv.Type().Elem()).Elem())
		fv.Set(m)
		filled++
	}
	require.Equal(t, len(known), filled, "every map v0.10.1 knows is still in the document")

	raw, _, err := encodeLearnedState(s, nil, time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), learnedStateMaxBytes)
	require.NoError(t, err)
	var doc any
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	assertKnownTo(t, doc, reflect.TypeFor[v0101LearnedStateDoc](), "doc")

	// And an empty state writes nothing v0.10.1 does not know.
	raw, _, err = encodeLearnedState(saturation.LearnedState{}, nil, time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), learnedStateMaxBytes)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	assertKnownTo(t, doc, reflect.TypeFor[v0101LearnedStateDoc](), "doc")
}

// The frozen types are v0.10.1's and nothing else: their JSON tags, type by
// type, are pinned to the list read off the tag. An edit to the copy fails
// here.
func TestV0101Decoder_TagSetIsPinned(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeFor[v0101LearnedStateDoc](),
		reflect.TypeFor[v0101LearnedStateWriter](),
		reflect.TypeFor[v0101LearnedState](),
		reflect.TypeFor[v0101RollingAverageSnapshot](),
		reflect.TypeFor[v0101PriorK2Snapshot](),
		reflect.TypeFor[v0101ThroughputSnapshot](),
		reflect.TypeFor[v0101SlopeSnapshot](),
		reflect.TypeFor[v0101AcceleratorSnapshot](),
		reflect.TypeFor[v0101LowerBoundSnapshot](),
		reflect.TypeFor[v0101LineSnapshot](),
		reflect.TypeFor[v0101ForgetMarkSnapshot](),
	}
	got := make([]string, 0, len(types))
	for _, typ := range types {
		var tags []string
		for i := range typ.NumField() {
			f := typ.Field(i)
			if f.Anonymous {
				tags = append(tags, "<"+f.Type.Name()+">")
				continue
			}
			tags = append(tags, f.Name+":"+f.Type.String()+":"+f.Tag.Get("json"))
		}
		got = append(got, typ.Name()+" "+strings.Join(tags, " "))
	}
	sort.Strings(got)
	want := []string{
		"v0101AcceleratorSnapshot Name:string:name LastUsed:time.Time:lastUsed",
		"v0101ForgetMarkSnapshot Token:string:token Seen:time.Time:seen",
		"v0101LearnedState K2:map[string]steadystate.v0101RollingAverageSnapshot:k2,omitempty " +
			"Throughput:map[string]steadystate.v0101ThroughputSnapshot:throughput,omitempty " +
			"ITLBaseline:map[string]float64:itlBaseline,omitempty " +
			"ITLBaselineStash:map[string]float64:itlBaselineStash,omitempty " +
			"ITLSlopes:map[string]steadystate.v0101SlopeSnapshot:itlSlopes,omitempty " +
			"StartSeconds:map[string]float64:startSeconds,omitempty " +
			"VariantSeenAt:map[string]time.Time:variantSeenAt,omitempty " +
			"Accelerators:map[string]steadystate.v0101AcceleratorSnapshot:accelerators,omitempty " +
			"ITLLines:map[string]steadystate.v0101LineSnapshot:itlLines,omitempty " +
			"K2BorrowedSince:map[string]time.Time:k2BorrowedSince,omitempty " +
			"LowerBounds:map[string]steadystate.v0101LowerBoundSnapshot:lowerBounds,omitempty " +
			"K2Args:map[string]string:k2Args,omitempty " +
			"ThroughputArgs:map[string]string:throughputArgs,omitempty " +
			"K2Stash:map[string]steadystate.v0101RollingAverageSnapshot:k2Stash,omitempty " +
			"ThroughputStash:map[string]steadystate.v0101ThroughputSnapshot:throughputStash,omitempty " +
			"Forgotten:map[string]steadystate.v0101ForgetMarkSnapshot:forgotten,omitempty " +
			"Config:map[string]string:config,omitempty " +
			"Args:map[string]string:args,omitempty " +
			"PriorK2:map[string]steadystate.v0101PriorK2Snapshot:priorK2,omitempty",
		"v0101LearnedStateDoc Version:int:version WrittenAt:time.Time:writtenAt " +
			"Writer:steadystate.v0101LearnedStateWriter:writer State:steadystate.v0101LearnedState:state",
		"v0101LearnedStateWriter Namespace:string:namespace Identity:string:identity",
		"v0101LineSnapshot A:float64:a B:float64:b FromVariant:string:fromVariant LearnedAt:time.Time:learnedAt",
		"v0101LowerBoundSnapshot Armed:bool:armed,omitempty Since:time.Time:since,omitzero " +
			"Tick:time.Time:tick,omitzero Seen:time.Time:seen Donor:string:donor,omitempty " +
			"Own:bool:own,omitempty DonorK2:int64:donorK2,omitempty Args:string:args,omitempty " +
			"ArgsVersion:int:argsVersion,omitempty Offered:int64:offered,omitempty " +
			"Prior:int64:prior,omitempty PriorSince:time.Time:priorSince,omitzero " +
			"PriorAgeSeconds:float64:priorAgeSeconds,omitempty PriorUsed:time.Time:priorUsed,omitzero " +
			"FigureWritten:time.Time:figureWritten,omitzero RearmPending:bool:rearmPending,omitempty",
		"v0101PriorK2Snapshot K2:int64:k2 InstalledAt:time.Time:installedAt",
		"v0101RollingAverageSnapshot Values:[]float64:values LastUsed:time.Time:lastUsed LastWritten:time.Time:lastWritten",
		"v0101SlopeSnapshot A:float64:a FittedAt:time.Time:fittedAt Args:string:args,omitempty",
		"v0101ThroughputSnapshot <v0101RollingAverageSnapshot> SampledAt:time.Time:sampledAt " +
			"LastRead:float64:lastRead HeldSeconds:float64:heldSeconds,omitempty",
	}
	assert.Equal(t, want, got)
}
