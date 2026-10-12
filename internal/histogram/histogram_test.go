package histogram

import (
	"encoding/json"
	"math"
	"math/big"
	"math/rand/v2"
	"slices"
	"testing"
)

// inBucket checks base^i < v <= base^(i+1) exactly: 2^i < v^(2^s) <= 2^(i+1)
// for s > 0, and the equivalent power-of-two comparison for s <= 0.
func inBucket(t *testing.T, v uint64, i, scale int32) bool {
	t.Helper()
	if scale <= 0 {
		// Bucket i at scale s covers (2^(i*2^-s), 2^((i+1)*2^-s)].
		w := int64(1) << -scale
		lo := new(big.Int).Lsh(big.NewInt(1), uint(max(int64(i)*w, 0)))
		hi := new(big.Int).Lsh(big.NewInt(1), uint(max((int64(i)+1)*w, 0)))
		x := new(big.Int).SetUint64(v)
		lowerOK := int64(i)*w < 0 || x.Cmp(lo) > 0
		if int64(i)*w < 0 {
			lowerOK = true // 2^negative < 1 <= v
		}
		return lowerOK && x.Cmp(hi) <= 0
	}
	x := new(big.Int).SetUint64(v)
	x.Exp(x, big.NewInt(int64(1)<<scale), nil)
	lo := new(big.Int).Lsh(big.NewInt(1), uint(max(i, 0)))
	hi := new(big.Int).Lsh(big.NewInt(1), uint(max(i+1, 0)))
	lowerOK := x.Cmp(lo) > 0 || i < 0
	return lowerOK && x.Cmp(hi) <= 0
}

func TestIndexIsExactAtBoundaries(t *testing.T) {
	for scale := int32(MinScale); scale <= MaxScale; scale++ {
		vals := []uint64{1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 255, 256, 257, 1000, 1023, 1024, 1025,
			1 << 20, 1<<20 + 1, 1<<32 - 1, 1 << 32, 59_999_999, 60_000_000}
		for _, v := range vals {
			i := Index(v, scale)
			if !inBucket(t, v, i, scale) {
				t.Errorf("scale %d: Index(%d) = %d, but v is not in that bucket", scale, v, i)
			}
		}
	}
}

func TestIndexMatchesDownscaling(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		v := r.Uint64N(1<<40) + 1
		for s := int32(MaxScale); s > MinScale; s-- {
			if Index(v, s)>>1 != Index(v, s-1) {
				t.Fatalf("Index(%d, %d)>>1 = %d, Index at %d = %d", v, s, Index(v, s)>>1, s-1, Index(v, s-1))
			}
		}
	}
}

func record(vals []uint64, scale int32) *Histogram {
	h := New(scale)
	for _, v := range vals {
		h.Record(v)
	}
	return h
}

func randVals(r *rand.Rand, n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		switch r.IntN(10) {
		case 0:
			out[i] = 0
		case 1:
			out[i] = r.Uint64N(10)
		default:
			// Latency-like: log-uniform between 100 µs and 10 s.
			out[i] = uint64(100 * math.Pow(1e5, r.Float64()))
		}
	}
	return out
}

func TestMergeEqualsRecordingEverything(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for trial := range 200 {
		a, b := randVals(r, r.IntN(300)), randVals(r, r.IntN(300))
		ha, hb := record(a, DefaultScale), record(b, DefaultScale)
		ha.Merge(hb)
		want := record(append(slices.Clone(a), b...), DefaultScale)
		// Downscaling may have happened in either path; compare at a common scale.
		x, y := ha.Clone(), want.Clone()
		if x.Scale > y.Scale {
			x.downscale(x.Scale - y.Scale)
		} else {
			y.downscale(y.Scale - x.Scale)
		}
		if !Equal(x, y) {
			t.Fatalf("trial %d: merge differs from recording all values", trial)
		}
	}
}

func TestMergeIsCommutativeAndAssociative(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for range 200 {
		a := record(randVals(r, r.IntN(200)), int32(r.IntN(6)))
		b := record(randVals(r, r.IntN(200)), int32(r.IntN(6)))
		c := record(randVals(r, r.IntN(200)), int32(r.IntN(6)))

		ab := a.Clone()
		ab.Merge(b)
		ba := b.Clone()
		ba.Merge(a)
		if !Equal(ab, ba) {
			t.Fatal("merge is not commutative")
		}
		left := a.Clone()
		left.Merge(b)
		left.Merge(c)
		bc := b.Clone()
		bc.Merge(c)
		right := a.Clone()
		right.Merge(bc)
		if !Equal(left, right) {
			t.Fatal("merge is not associative")
		}
		for _, h := range []*Histogram{ab, left, right} {
			if err := h.Validate(); err != nil {
				t.Fatalf("merged histogram invalid: %v", err)
			}
		}
	}
}

func TestMergeEmptyAndIntoEmpty(t *testing.T) {
	h := New(DefaultScale)
	h.Merge(New(DefaultScale))
	if h.Count() != 0 {
		t.Fatal("merging empty histograms must stay empty")
	}
	o := record([]uint64{5, 50, 500}, DefaultScale)
	h.Merge(o)
	if !Equal(h, o) {
		t.Fatal("merging into empty must copy")
	}
	h.Merge(nil)
	if h.Count() != 3 {
		t.Fatal("merging nil must be a no-op")
	}
}

func TestQuantileBoundsContainTheTrueValue(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	for range 300 {
		vals := randVals(r, 1+r.IntN(500))
		h := record(vals, DefaultScale)
		sorted := slices.Clone(vals)
		slices.Sort(sorted)
		for _, pm := range []uint64{1, 500, 900, 950, 990, 999, 1000} {
			q, ok := h.QuantilePermille(pm)
			if !ok {
				t.Fatal("quantile of non-empty histogram not ok")
			}
			// Nearest rank: ceil(p*n).
			n := uint64(len(vals))
			rank := (pm*n + 999) / 1000
			if q.Rank != rank {
				t.Fatalf("rank = %d, want %d", q.Rank, rank)
			}
			truth := sorted[rank-1]
			if q.Zero {
				if truth != 0 {
					t.Fatalf("p%d: zero bucket but true value %d", pm, truth)
				}
				continue
			}
			if !(truth > q.Lower && truth <= q.Upper) {
				t.Fatalf("p%d: true value %d outside (%d, %d]", pm, truth, q.Lower, q.Upper)
			}
		}
	}
}

func TestQuantileEdgeCases(t *testing.T) {
	if _, ok := New(DefaultScale).QuantilePermille(500); ok {
		t.Fatal("quantile of empty histogram must not be ok")
	}
	h := record([]uint64{7}, DefaultScale)
	for _, pm := range []uint64{0, 1001} {
		if _, ok := h.QuantilePermille(pm); ok {
			t.Fatalf("permille %d must be rejected", pm)
		}
	}
	q, _ := h.QuantilePermille(990)
	if q.Upper != 7 || q.Lower >= 7 {
		t.Fatalf("single value 7: bounds (%d, %d]", q.Lower, q.Upper)
	}
	z := record([]uint64{0, 0, 0, 9}, DefaultScale)
	if q, _ := z.QuantilePermille(750); !q.Zero {
		t.Fatal("p75 of {0,0,0,9} is 0")
	}
	if q, _ := z.QuantilePermille(760); q.Zero || q.Upper != 9 {
		t.Fatalf("p76 of {0,0,0,9} is 9, got %+v", q)
	}
}

func TestDownscaleKeepsBucketLimit(t *testing.T) {
	h := New(MaxScale)
	h.MaxBuckets = 20
	for v := uint64(1); v < 1<<30; v *= 3 {
		h.Record(v)
	}
	if len(h.Counts) > 20 {
		t.Fatalf("%d buckets exceed the limit", len(h.Counts))
	}
	if h.Scale >= MaxScale {
		t.Fatal("expected the histogram to downscale")
	}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCountAtMost(t *testing.T) {
	h := record([]uint64{0, 10, 100, 1000, 10000}, DefaultScale)
	certain, possible := h.CountAtMost(1000)
	if certain != 3 || possible != 4 {
		t.Fatalf("CountAtMost(1000) = %d, %d; want 3 certain (0, 10, 100), 4 possible", certain, possible)
	}
	if c, p := h.CountAtMost(0); c != 1 || p != 1 {
		t.Fatalf("CountAtMost(0) = %d, %d", c, p)
	}
}

func TestValidateRejectsInconsistentData(t *testing.T) {
	good := record([]uint64{3, 30, 300}, DefaultScale)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(h *Histogram){
		"scale":         func(h *Histogram) { h.Scale = 99 },
		"min>max":       func(h *Histogram) { h.Min, h.Max = 400, 3 },
		"sum too large": func(h *Histogram) { h.Sum = 1 << 40 },
		"sum too small": func(h *Histogram) { h.Sum = 1 },
		"max bucket":    func(h *Histogram) { h.Max = 3000 },
		"min bucket":    func(h *Histogram) { h.Min = 1 },
		"empty with sum": func(h *Histogram) {
			h.Counts, h.ZeroCount = nil, 0
		},
	}
	for name, mut := range cases {
		h := good.Clone()
		mut(h)
		if err := h.Validate(); err == nil {
			t.Errorf("%s: Validate accepted an inconsistent histogram", name)
		}
	}
}

func TestJSONRoundTrip(t *testing.T) {
	h := record([]uint64{0, 1, 2, 999, 123456}, DefaultScale)
	h.Normalize()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back Histogram
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !Equal(h, &back) || back.Validate() != nil {
		t.Fatal("JSON round trip changed the histogram")
	}
}

func FuzzRecordAndMerge(f *testing.F) {
	f.Add(uint64(0), uint64(1), uint64(1000), uint8(3))
	f.Add(uint64(1<<63), uint64(7), uint64(0), uint8(0))
	f.Fuzz(func(t *testing.T, a, b, c uint64, s uint8) {
		scale := int32(s%uint8(MaxScale-MinScale+1)) + MinScale
		// Keep values in a latency-like range so sums cannot overflow.
		a, b, c = a%(1<<40), b%(1<<40), c%(1<<40)
		h1 := record([]uint64{a, b}, scale)
		h2 := record([]uint64{c}, DefaultScale)
		h1.Merge(h2)
		if h1.Count() != 3 {
			t.Fatalf("count %d", h1.Count())
		}
		if err := h1.Validate(); err != nil {
			t.Fatalf("invalid after merge: %v", err)
		}
		if q, ok := h1.QuantilePermille(1000); !ok || (!q.Zero && q.Upper != max(a, b, c)) {
			t.Fatalf("p100 = %+v, want max %d", q, max(a, b, c))
		}
	})
}

func FuzzDecodeValidate(f *testing.F) {
	f.Add([]byte(`{"scale":3,"zero_count":1,"offset":10,"counts":[1,2],"sum":100,"min":0,"max":30}`))
	f.Add([]byte(`{"scale":-9,"counts":[1]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var h Histogram
		if json.Unmarshal(data, &h) != nil {
			return
		}
		if h.Validate() != nil {
			return
		}
		// A valid histogram must support every read operation without panicking.
		h.QuantilePermille(500)
		h.Normalize()
		c := h.Clone()
		c.Merge(&h)
		if c.Count() != 2*h.Count() {
			t.Fatalf("self-merge count %d, want %d", c.Count(), 2*h.Count())
		}
	})
}
