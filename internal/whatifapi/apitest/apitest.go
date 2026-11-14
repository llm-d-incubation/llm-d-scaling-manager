// Package apitest holds the checks every whatifapi response must pass, for
// the API's own tests and cmd/capacity-planner's, which run them on the goldens.
package apitest

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// CheckFigures walks a response body and returns every place it breaks the
// wire rule: an object holding "refused" that holds a number too, a value
// that is not finite, or a priced "value" of exactly 0 (a refusal written as
// a number). A headroom or a delta, a difference, may be 0, and so may an
// input's term (a stated router hop of 0, a prompt cv of 0).
func CheckFigures(body []byte) []string {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return []string{fmt.Sprintf("not JSON: %v", err)}
	}
	var bad []string
	walk(v, "", &bad)
	return bad
}

// zeroAllowed are the keys whose figure may be exactly 0: a difference.
var zeroAllowed = []string{"headroom", "delta"}

func walk(v any, path string, bad *[]string) {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["refused"]; ok {
			for k, x := range t {
				if _, isNum := x.(json.Number); isNum {
					*bad = append(*bad, fmt.Sprintf("%s: %q = %v beside a refusal", path, k, x))
				}
			}
		}
		if n, ok := t["value"].(json.Number); ok {
			f, err := n.Float64()
			last := path[strings.LastIndex(path, ".")+1:]
			switch {
			case err != nil || math.IsNaN(f) || math.IsInf(f, 0):
				*bad = append(*bad, fmt.Sprintf("%s: value %v is not finite", path, n))
			case f == 0 && !slices.Contains(zeroAllowed, last) && !strings.HasPrefix(path, ".terms[") && !strings.HasPrefix(path, ".observed."):
				*bad = append(*bad, path+": value 0, where a refusal would be written as one")
			}
		}
		for k, x := range t {
			walk(x, path+"."+k, bad)
		}
	case []any:
		for i, x := range t {
			walk(x, path+"["+strconv.Itoa(i)+"]", bad)
		}
	}
}
