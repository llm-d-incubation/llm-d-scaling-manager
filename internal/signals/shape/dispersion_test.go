package shape

import (
	"math"
	"testing"
)

func TestDispersion(t *testing.T) {
	for _, c := range []struct{ mean, sq, want float64 }{
		{500, 500 * 500, 1},
		{500, 2 * 500 * 500, 2},
		{500, 100, 1}, // below mean^2: rounding, read as 1
		{0, 100, 1},
		{500, 0, 1},
		{math.NaN(), 1, 1},
		{500, math.Inf(1), 1},
	} {
		if got := Dispersion(c.mean, c.sq); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("Dispersion(%v, %v) = %v, want %v", c.mean, c.sq, got, c.want)
		}
	}
}

// The shape prices the spread into the generated half only.
func TestShape_WithOutputDispersion(t *testing.T) {
	s := New(1000, 500, 0.2)
	if s.OutputDispersion != 1 || s.KVreq != 800+250 {
		t.Fatalf("New: %+v", s)
	}
	if got := s.WithOutputDispersion(2).KVreq; got != 800+500 {
		t.Fatalf("D 2: KVreq %v, want 1300", got)
	}
	for _, d := range []float64{0.5, math.NaN(), math.Inf(1), 0} {
		if got := s.WithOutputDispersion(d); got.KVreq != s.KVreq || got.OutputDispersion != 1 {
			t.Errorf("D %v: %+v, want the shape as New prices it", d, got)
		}
	}
}
