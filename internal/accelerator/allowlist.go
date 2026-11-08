package accelerator

import (
	"fmt"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// An ALLOWLIST is a set of GPU bounds keyed by accelerator name: a quota's
// per-type caps, or a provider's per-type pools. It is closed: a name it does
// not list gets no GPUs under it. The functions below are the one reading of an
// allowlist, shared by the budget merge (the allocation package) and the
// configuration warnings (the config package), so the two cannot disagree on
// which key bounds which name.

// Bound is one allowlist value read as a GPU bound: the GPUs it leaves free, or
// no bound at all.
type Bound struct {
	// Free is the number of GPUs the bound leaves. Ignored when Unlimited.
	Free int
	// Unlimited is true for the -1 sentinel: no bound.
	Unlimited bool
}

// AllowlistBound is what one allowlist allows on the accelerator called name:
// the free GPUs, whether that is unlimited, and whether it lists name at all.
// bound reads one value of the allowlist.
//
//   - A key equal to name ignoring case is its bound; for a product name, a
//     listed family key bounds it too.
//   - A family name listed only through products is bounded by their SUM: an
//     allowlist of {NVIDIA-H100-80GB-HBM3: 4, NVIDIA-H100-SXM5-80GB: 4} allows
//     8 H100s, not 4, and is unlimited when one of them is.
//   - A product name not listed itself falls back to its family key, which
//     admits every product under it (FindKey's reading).
//
// Several case spellings of one key take the tightest.
func AllowlistBound[V any](allowlist map[string]V, name string, bound func(V) Bound) (free int, unlimited, listed bool) {
	var own, family, products []Bound
	for key, v := range allowlist {
		switch {
		case strings.EqualFold(key, name):
			own = append(own, bound(v))
		case IsProductOf(key, name):
			products = append(products, bound(v))
		case IsProductOf(name, key):
			family = append(family, bound(v))
		}
	}
	switch {
	case len(own) > 0:
		free, unlimited = tightest(own)
		if len(family) > 0 {
			ff, fu := tightest(family)
			free, unlimited = tighter(free, unlimited, ff, fu)
		}
	case len(products) > 0:
		for _, b := range products {
			if b.Unlimited {
				return 0, true, true
			}
			free += b.Free
		}
	case len(family) > 0:
		free, unlimited = tightest(family)
	default:
		return 0, false, false
	}
	return free, unlimited, true
}

// ClusterAllowlistBound is the bound one cluster-wide allowlist (the physical
// inventory, a scope: cluster quota) puts on the accelerator called name, and
// whether it bounds name at all. It is AllowlistBound, with a name it does not
// list bounded at 0 (a closed allowlist), except the catch-all "unknown"
// (constants.DefaultAcceleratorName): no physical inventory lists it, so an
// allowlist that does not list it does not bound it.
func ClusterAllowlistBound[V any](allowlist map[string]V, name string, bound func(V) Bound) (free int, unlimited, bounds bool) {
	free, unlimited, listed := AllowlistBound(allowlist, name, bound)
	switch {
	case !listed && name == constants.DefaultAcceleratorName:
		return 0, false, false
	case !listed:
		return 0, false, true
	}
	return free, unlimited, true
}

// ClusterAllowlistsBound is the bound several cluster-wide allowlists put
// together on the accelerator called name: the GPUs that fit under every one
// of them, and whether any of them bounds name. Each is read by
// ClusterAllowlistBound, and the tightest wins.
//
// A family name needs more than the tightest. When one of the allowlists does
// not list the family itself, the family's GPUs can only be products that
// allowlist lists, so the family is also bounded by the sum, over every
// product any of them lists, of the tightest bound on that product. Two
// allowlists of {NVIDIA-H100-80GB-HBM3: 8, NVIDIA-H100-PCIE-80GB: 2} and
// {NVIDIA-H100-80GB-HBM3: 2, NVIDIA-H100-PCIE-80GB: 8} each allow 10 H100s,
// but only 2 of each product fit under both: 4.
func ClusterAllowlistsBound[V any](allowlists []map[string]V, name string, bound func(V) Bound) (free int, unlimited, bounds bool) {
	tightestOf := func(name string) (free int, unlimited, bounds bool) {
		unlimited = true
		for _, a := range allowlists {
			f, u, b := ClusterAllowlistBound(a, name, bound)
			if !b {
				continue
			}
			free, unlimited = tighter(free, unlimited, f, u)
			bounds = true
		}
		return free, unlimited && bounds, bounds
	}
	free, unlimited, bounds = tightestOf(name)
	if !bounds || !ClosesFamily(allowlists, name) {
		return free, unlimited, bounds
	}
	var products []string
	for _, a := range allowlists {
		for key := range a {
			if IsProductOf(key, name) && !slices.ContainsFunc(products, func(p string) bool { return strings.EqualFold(p, key) }) {
				products = append(products, key)
			}
		}
	}
	sum, sumUnlimited := 0, false
	for _, p := range products {
		f, u, _ := tightestOf(p)
		if u {
			sumUnlimited = true
			break
		}
		sum += f
	}
	free, unlimited = tighter(free, unlimited, sum, sumUnlimited)
	return free, unlimited, true
}

// ClosesFamily reports whether name is a family that one of the allowlists
// does not list itself, ignoring case. That allowlist admits only the products
// of name it lists, so GPUs of the family can only be those products. Never
// true for an unresolved placeholder such as "unknown", which has no products.
func ClosesFamily[V any](allowlists []map[string]V, name string) bool {
	if !IsFamily(name) || !constants.IsAcceleratorResolved(name) {
		return false
	}
	return slices.ContainsFunc(allowlists, func(a map[string]V) bool {
		for key := range a {
			if strings.EqualFold(key, name) {
				return false
			}
		}
		return true
	})
}

// ListsProduct reports whether one of the allowlists lists a product of the
// family called name.
func ListsProduct[V any](allowlists []map[string]V, name string) bool {
	return slices.ContainsFunc(allowlists, func(a map[string]V) bool {
		for key := range a {
			if IsProductOf(key, name) {
				return true
			}
		}
		return false
	})
}

// UnlistedProduct returns a product name under family that no real accelerator
// reports, and whether family has one. Every product an allowlist does not list
// is read alike (AllowlistBound falls back to the family key for each), so this
// one name stands for all of them when asking what an allowlist allows them.
func UnlistedProduct(family string) (string, bool) {
	for _, vendor := range []string{"NVIDIA", "Intel", "AMD", "vendor"} {
		name := fmt.Sprintf("%s-%s-unlisted-product", vendor, family)
		if IsProductOf(name, family) {
			return name, true
		}
	}
	return "", false
}

// tightest is the smallest free figure among bounds, unlimited only when every
// one of them is.
func tightest(bounds []Bound) (free int, unlimited bool) {
	unlimited = true
	for _, b := range bounds {
		if b.Unlimited {
			continue
		}
		free, unlimited = tighter(free, unlimited, b.Free, false)
	}
	return free, unlimited
}

// tighter is the tighter of two (free, unlimited) bounds.
func tighter(a int, aUnlimited bool, b int, bUnlimited bool) (int, bool) {
	switch {
	case aUnlimited:
		return b, bUnlimited
	case bUnlimited:
		return a, false
	}
	return min(a, b), false
}
