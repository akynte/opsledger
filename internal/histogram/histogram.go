// Package histogram implements a mergeable base-2 exponential histogram for
// non-negative integer measurements (latencies in microseconds).
//
// The bucketing follows the OpenTelemetry exponential histogram data model:
// at scale s the base is 2^(2^-s), and bucket i holds values v with
// base^i < v <= base^(i+1). Values equal to zero go to a separate zero
// bucket.
//
// Unlike most implementations, bucket indices are computed with exact
// integer arithmetic instead of floating-point logarithms. The same input
// therefore lands in the same bucket on every machine, which is what makes
// published reports recomputable: a reviewer merging the published buckets
// gets bit-for-bit the same counts, and percentile ranks are found by
// integer arithmetic only.
//
// Histograms never average percentiles. Combining two histograms adds their
// bucket counts (after bringing them to a common scale), so percentiles of
// the combination are computed from the combined distribution.
package histogram

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/bits"
)

const (
	// MinScale and MaxScale bound the supported resolution. Scale 8 has a
	// relative bucket width of about 0.27%; scale -4 has buckets 2^16 wide.
	MinScale = -4
	MaxScale = 8
	// DefaultScale gives a base of 2^(1/8) ≈ 1.0905: any value is within
	// about 4.3% of its bucket's geometric midpoint.
	DefaultScale = 3
	// DefaultMaxBuckets bounds memory per histogram. At scale 3, 160
	// buckets span a ratio of 2^20 (for example 1 µs to about 1 s, or 60 µs
	// to 60 s) before the histogram has to downscale.
	DefaultMaxBuckets = 160
)

// Histogram is an exponential histogram. The zero value is not usable; use
// New. Fields are exported for encoding; use the methods to modify them.
type Histogram struct {
	Scale      int32    `json:"scale"`
	ZeroCount  uint64   `json:"zero_count"`
	Offset     int32    `json:"offset"`
	Counts     []uint64 `json:"counts"`
	Sum        uint64   `json:"sum"`
	Min        uint64   `json:"min"`
	Max        uint64   `json:"max"`
	MaxBuckets int      `json:"-"`
}

// New returns an empty histogram at the given scale.
func New(scale int32) *Histogram {
	if scale < MinScale || scale > MaxScale {
		panic(fmt.Sprintf("histogram: scale %d out of range [%d, %d]", scale, MinScale, MaxScale))
	}
	return &Histogram{Scale: scale, MaxBuckets: DefaultMaxBuckets}
}

// Count is the number of recorded values.
func (h *Histogram) Count() uint64 {
	n := h.ZeroCount
	for _, c := range h.Counts {
		n += c
	}
	return n
}

// Record adds one value.
func (h *Histogram) Record(v uint64) { h.RecordN(v, 1) }

// RecordN adds a value n times.
func (h *Histogram) RecordN(v, n uint64) {
	if n == 0 {
		return
	}
	if h.Count() == 0 {
		h.Min, h.Max = v, v
	} else {
		h.Min, h.Max = min(h.Min, v), max(h.Max, v)
	}
	h.Sum += v * n
	if v == 0 {
		h.ZeroCount += n
		return
	}
	idx := Index(v, h.Scale)
	for !h.fits(idx) {
		h.downscale(1)
		idx = Index(v, h.Scale)
	}
	h.add(idx, n)
}

func (h *Histogram) maxBuckets() int {
	if h.MaxBuckets <= 0 {
		return DefaultMaxBuckets
	}
	return h.MaxBuckets
}

// fits reports whether index idx can be added without exceeding the bucket limit.
func (h *Histogram) fits(idx int32) bool {
	if len(h.Counts) == 0 {
		return true
	}
	lo, hi := min(h.Offset, idx), max(h.Offset+int32(len(h.Counts))-1, idx)
	return int(hi-lo)+1 <= h.maxBuckets() || h.Scale == MinScale
}

func (h *Histogram) add(idx int32, n uint64) {
	switch {
	case len(h.Counts) == 0:
		h.Offset = idx
		h.Counts = []uint64{n}
	case idx < h.Offset:
		grown := make([]uint64, int(h.Offset-idx)+len(h.Counts))
		copy(grown[h.Offset-idx:], h.Counts)
		h.Counts, h.Offset = grown, idx
		h.Counts[0] += n
	case int(idx-h.Offset) >= len(h.Counts):
		grown := make([]uint64, int(idx-h.Offset)+1)
		copy(grown, h.Counts)
		h.Counts = grown
		h.Counts[idx-h.Offset] += n
	default:
		h.Counts[idx-h.Offset] += n
	}
}

// downscale lowers the scale by d, merging adjacent buckets.
func (h *Histogram) downscale(d int32) {
	if d <= 0 {
		return
	}
	if h.Scale-d < MinScale {
		d = h.Scale - MinScale
		if d == 0 {
			return
		}
	}
	h.Scale -= d
	if len(h.Counts) == 0 {
		return
	}
	newOffset := h.Offset >> d
	last := (h.Offset + int32(len(h.Counts)) - 1) >> d
	out := make([]uint64, int(last-newOffset)+1)
	for i, c := range h.Counts {
		out[((h.Offset+int32(i))>>d)-newOffset] += c
	}
	h.Counts, h.Offset = out, newOffset
}

// Merge adds o into h. The result has the highest scale, no higher than
// either input's, at which the union of both ranges fits the bucket limit.
// That scale does not depend on the order of merges, so merging is
// commutative and associative. o is unchanged.
func (h *Histogram) Merge(o *Histogram) {
	if o == nil || o.Count() == 0 {
		return
	}
	src := o.Clone()
	if h.Count() == 0 {
		// An empty histogram carries no data, so its scale must not lower the result's.
		h.Scale, h.Counts, h.Offset, h.ZeroCount, h.Sum = src.Scale, nil, 0, 0, 0
	}
	target := min(h.Scale, src.Scale)
	if len(h.Counts) > 0 && len(src.Counts) > 0 {
		for target > MinScale {
			lo := min(h.Offset>>(h.Scale-target), src.Offset>>(src.Scale-target))
			hi := max((h.Offset+int32(len(h.Counts))-1)>>(h.Scale-target),
				(src.Offset+int32(len(src.Counts))-1)>>(src.Scale-target))
			if int(hi-lo)+1 <= h.maxBuckets() {
				break
			}
			target--
		}
	}
	h.downscale(h.Scale - target)
	src.downscale(src.Scale - target)
	if h.Count() == 0 {
		h.Min, h.Max = src.Min, src.Max
	} else {
		h.Min, h.Max = min(h.Min, src.Min), max(h.Max, src.Max)
	}
	h.Sum += src.Sum
	h.ZeroCount += src.ZeroCount
	for i, c := range src.Counts {
		if c != 0 {
			h.add(src.Offset+int32(i), c)
		}
	}
}

// Clone returns a deep copy.
func (h *Histogram) Clone() *Histogram {
	c := *h
	c.Counts = append([]uint64(nil), h.Counts...)
	return &c
}

// Normalize trims empty leading and trailing buckets so that equal
// distributions have equal encodings.
func (h *Histogram) Normalize() {
	first, last := 0, len(h.Counts)-1
	for first <= last && h.Counts[first] == 0 {
		first++
	}
	for last >= first && h.Counts[last] == 0 {
		last--
	}
	if first > last {
		h.Counts, h.Offset = nil, 0
		return
	}
	h.Counts = append([]uint64(nil), h.Counts[first:last+1]...)
	h.Offset += int32(first)
}

// Validate checks the internal consistency of a decoded histogram.
func (h *Histogram) Validate() error {
	if h.Scale < MinScale || h.Scale > MaxScale {
		return fmt.Errorf("scale %d out of range [%d, %d]", h.Scale, MinScale, MaxScale)
	}
	if len(h.Counts) > 4096 {
		return fmt.Errorf("too many buckets (%d)", len(h.Counts))
	}
	n := h.Count()
	if n == 0 {
		if h.Sum != 0 || h.Min != 0 || h.Max != 0 {
			return errors.New("empty histogram with nonzero sum, min or max")
		}
		return nil
	}
	if h.Min > h.Max {
		return fmt.Errorf("min %d > max %d", h.Min, h.Max)
	}
	// Every value is between min and max, so the sum is too (modulo overflow, which
	// latencies in microseconds cannot reach).
	if hi, lo := bits.Mul64(h.Max, n); hi == 0 && h.Sum > lo {
		return fmt.Errorf("sum %d exceeds max*count", h.Sum)
	}
	if hi, lo := bits.Mul64(h.Min, n); hi == 0 && h.Sum < lo {
		return fmt.Errorf("sum %d below min*count", h.Sum)
	}
	if h.ZeroCount > 0 && h.Min != 0 {
		return errors.New("zero bucket is not empty but min > 0")
	}
	if len(h.Counts) > 0 {
		// The extreme values must fall into the outermost non-empty buckets' range.
		first, last := -1, -1
		for i, c := range h.Counts {
			if c > 0 {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		if first >= 0 {
			if h.Max == 0 || Index(h.Max, h.Scale) != h.Offset+int32(last) {
				return fmt.Errorf("max %d is not in the highest non-empty bucket", h.Max)
			}
			if h.ZeroCount == 0 && (h.Min == 0 || Index(h.Min, h.Scale) != h.Offset+int32(first)) {
				return fmt.Errorf("min %d is not in the lowest non-empty bucket", h.Min)
			}
		}
	}
	return nil
}

// Equal reports whether two normalized histograms hold the same data.
func Equal(a, b *Histogram) bool {
	if a.Scale != b.Scale || a.ZeroCount != b.ZeroCount || a.Sum != b.Sum || a.Min != b.Min || a.Max != b.Max {
		return false
	}
	x, y := a.Clone(), b.Clone()
	x.Normalize()
	y.Normalize()
	if x.Offset != y.Offset || len(x.Counts) != len(y.Counts) {
		return false
	}
	for i := range x.Counts {
		if x.Counts[i] != y.Counts[i] {
			return false
		}
	}
	return true
}

// Index returns the bucket index of v > 0 at the given scale, computed
// exactly: the i with base^i < v <= base^(i+1), which is
// ceil(log2(v) * 2^scale) - 1.
func Index(v uint64, scale int32) int32 {
	if v == 0 {
		panic("histogram: Index of zero")
	}
	if scale <= 0 {
		// ceil(log2 v) - 1 at scale 0, then floor-divide by 2^-scale.
		i := int32(bits.Len64(v-1)) - 1
		return i >> -scale
	}
	// ceil(log2(v^(2^scale))) - 1, and ceil(log2 x) = bitlen(x-1) for x >= 1.
	x := new(big.Int).SetUint64(v)
	x.Exp(x, big.NewInt(int64(1)<<scale), nil)
	x.Sub(x, big.NewInt(1))
	return int32(x.BitLen()) - 1
}

// LowerBound and UpperBound return the bucket's boundaries base^i and
// base^(i+1). They are floating point and for display only; recomputation
// compares indices, never these values.
func LowerBound(i, scale int32) float64 {
	return math.Exp2(float64(i) / math.Exp2(float64(scale)))
}

// UpperBound returns base^(i+1).
func UpperBound(i, scale int32) float64 { return LowerBound(i+1, scale) }

// Quantile is a nearest-rank quantile located in a bucket.
type Quantile struct {
	// Rank is the 1-based rank ceil(q*n) of the value in the sorted data.
	Rank uint64 `json:"rank"`
	// Zero is true when the value is in the zero bucket.
	Zero bool `json:"zero"`
	// Index is the bucket index (meaningless when Zero).
	Index int32 `json:"index"`
	// Lower and Upper bound the value: Lower < value <= Upper (both 0 when Zero).
	// They are rounded to whole units (microseconds) for publication.
	Lower uint64 `json:"lower"`
	Upper uint64 `json:"upper"`
}

// QuantilePermille returns the nearest-rank quantile q = permille/1000,
// using integer arithmetic. ok is false for an empty histogram.
func (h *Histogram) QuantilePermille(permille uint64) (q Quantile, ok bool) {
	n := h.Count()
	if n == 0 || permille == 0 || permille > 1000 {
		return Quantile{}, false
	}
	hi, lo := bits.Mul64(permille, n)
	quo, rem := bits.Div64(hi, lo, 1000)
	rank := quo
	if rem != 0 {
		rank++
	}
	if rank == 0 {
		rank = 1
	}
	q.Rank = rank
	if rank <= h.ZeroCount {
		q.Zero = true
		return q, true
	}
	cum := h.ZeroCount
	for i, c := range h.Counts {
		cum += c
		if cum >= rank {
			idx := h.Offset + int32(i)
			q.Index = idx
			q.Lower = uint64(math.Floor(LowerBound(idx, h.Scale)))
			q.Upper = uint64(math.Ceil(UpperBound(idx, h.Scale)))
			// The recorded extremes tighten the bounds.
			q.Upper = min(q.Upper, h.Max)
			if h.Min > 0 && h.Min-1 > q.Lower {
				q.Lower = h.Min - 1
			}
			if q.Lower >= q.Upper {
				q.Lower = q.Upper - 1
			}
			return q, true
		}
	}
	// Unreachable for a consistent histogram.
	return Quantile{}, false
}

// CountAtMost returns how many recorded values are certainly <= v, and how
// many may be (values in the bucket containing v). Use it for threshold
// SLIs only when the exact count was not recorded at ingestion.
func (h *Histogram) CountAtMost(v uint64) (certain, possible uint64) {
	certain = h.ZeroCount
	if v == 0 {
		return certain, certain
	}
	vi := Index(v, h.Scale)
	possible = certain
	for i, c := range h.Counts {
		idx := h.Offset + int32(i)
		switch {
		case idx < vi:
			certain += c
			possible += c
		case idx == vi:
			possible += c
		}
	}
	return certain, possible
}
