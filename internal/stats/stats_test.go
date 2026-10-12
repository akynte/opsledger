package stats

import (
	"math"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// Reference values computed independently (Wilson score interval, z = 1.96).
func TestWilsonReferenceValues(t *testing.T) {
	cases := []struct {
		n, good      uint64
		lower, upper float64
	}{
		{10, 9, 0.5958, 0.9821},
		{100, 95, 0.8882, 0.9785},
		{1000, 999, 0.99437, 0.99982},
		{20, 0, 0, 0.1611},
		{20, 20, 0.8389, 1},
	}
	for _, c := range cases {
		p := NewProportion(c.n, c.good)
		if !near(p.Lower, c.lower, 1e-4) || !near(p.Upper, c.upper, 1e-4) {
			t.Errorf("n=%d good=%d: [%v, %v], want [%v, %v]", c.n, c.good, p.Lower, p.Upper, c.lower, c.upper)
		}
		if p.Lower > p.Ratio || p.Ratio > p.Upper {
			t.Errorf("n=%d good=%d: ratio %v outside its interval", c.n, c.good, p.Ratio)
		}
	}
}

func TestProportionUndefinedWithoutObservations(t *testing.T) {
	p := NewProportion(0, 0)
	if p.Defined || p.Ratio != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestIntervalNarrowsWithMoreData(t *testing.T) {
	prev := 1.0
	for _, n := range []uint64{10, 100, 1000, 10000} {
		p := NewProportion(n, n*99/100)
		w := p.Upper - p.Lower
		if w >= prev {
			t.Fatalf("width %v at n=%d did not shrink", w, n)
		}
		prev = w
	}
}

func TestZeroFailureUpperBound(t *testing.T) {
	// 1 - 0.05^(1/n), and close to 3/n for large n.
	if got := ZeroFailureUpperBound(1344); !near(got, 0.002226, 2e-6) {
		t.Fatalf("n=1344: %v", got)
	}
	if got := ZeroFailureUpperBound(8064); !near(got, 3.0/8064, 1e-6) {
		t.Fatalf("n=8064: %v, want about %v", got, 3.0/8064)
	}
	if ZeroFailureUpperBound(0) != 1 {
		t.Fatal("no trials bound nothing")
	}
}

func TestBudget(t *testing.T) {
	b := NewBudget(0.995, 10000, 25)
	if !b.Defined || b.Allowed != 50 || b.Consumed != 0.5 || b.Remains != 0.5 {
		t.Fatalf("%+v", b)
	}
	over := NewBudget(0.995, 100, 2)
	if over.Consumed != 4 || over.Remains != -3 {
		t.Fatalf("overspent budget %+v", over)
	}
	if NewBudget(0.995, 0, 0).Defined {
		t.Fatal("empty window has no budget")
	}
}

func TestBurnRate(t *testing.T) {
	if got := BurnRate(0.999, 1000, 1); !near(got, 1, 1e-9) {
		t.Fatalf("burn %v", got)
	}
	if got := BurnRate(0.999, 1000, 14); !near(got, 14, 1e-9) {
		t.Fatalf("burn %v", got)
	}
}

func TestReliableQuantile(t *testing.T) {
	cases := map[[2]uint64]bool{{19, 500}: false, {20, 500}: true, {199, 950}: false, {200, 950}: true, {999, 990}: false, {1000, 990}: true}
	for in, want := range cases {
		if ReliableQuantile(in[0], in[1]) != want {
			t.Errorf("ReliableQuantile(%d, %d) != %v", in[0], in[1], want)
		}
	}
}
