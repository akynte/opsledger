// Package stats provides the small amount of statistics that published
// reports need: proportions with confidence intervals and error budgets.
//
// Every function takes and returns plain counts and float64s, rounds its
// published outputs to a fixed number of decimals, and is deterministic,
// so a reviewer recomputing a report gets the same digits.
package stats

import "math"

// Decimals is the precision of every published ratio.
const Decimals = 9

// Round rounds to Decimals places (half away from zero).
func Round(x float64) float64 {
	p := math.Pow(10, Decimals)
	return math.Round(x*p) / p
}

// z95 is the two-sided 95% standard normal quantile.
const z95 = 1.959963984540054

// Proportion is a success ratio with its 95% Wilson score interval.
type Proportion struct {
	N     uint64  `json:"n"`
	Good  uint64  `json:"good"`
	Bad   uint64  `json:"bad"`
	Ratio float64 `json:"ratio"` // Good/N; 0 when N == 0 (see Defined)
	// Lower and Upper bound the true success probability at 95% confidence
	// (Wilson score interval), treating observations as independent trials.
	Lower float64 `json:"wilson95_lower"`
	Upper float64 `json:"wilson95_upper"`
	// Defined is false when N == 0: nothing was observed.
	Defined bool `json:"defined"`
}

// NewProportion computes a Proportion from counts. good must not exceed n.
func NewProportion(n, good uint64) Proportion {
	p := Proportion{N: n, Good: good, Bad: n - good}
	if n == 0 {
		return p
	}
	p.Defined = true
	fn := float64(n)
	phat := float64(good) / fn
	z2 := z95 * z95
	den := 1 + z2/fn
	center := (phat + z2/(2*fn)) / den
	half := z95 * math.Sqrt(phat*(1-phat)/fn+z2/(4*fn*fn)) / den
	p.Ratio = Round(phat)
	p.Lower = Round(max(0, center-half))
	p.Upper = Round(min(1, center+half))
	// Exact endpoints when nothing (or everything) failed, avoiding rounding drift.
	if good == n {
		p.Upper = 1
	}
	if good == 0 {
		p.Lower = 0
	}
	return p
}

// ZeroFailureUpperBound is the exact one-sided 95% upper bound on the
// failure probability after n independent trials with no failures:
// 1 - 0.05^(1/n) (Clopper-Pearson). For large n it approaches the "rule
// of three", 3/n. It returns 1 for n == 0.
func ZeroFailureUpperBound(n uint64) float64 {
	if n == 0 {
		return 1
	}
	return Round(1 - math.Pow(0.05, 1/float64(n)))
}

// Budget describes an error budget over a window.
type Budget struct {
	Target   float64 `json:"target"`    // e.g. 0.995
	Eligible uint64  `json:"eligible"`  // events in the window
	Bad      uint64  `json:"bad"`       // bad events in the window
	Allowed  float64 `json:"allowed"`   // (1 - Target) * Eligible
	Consumed float64 `json:"consumed"`  // Bad / Allowed (1 = budget exhausted); 0 if nothing is allowed yet
	Remains  float64 `json:"remaining"` // 1 - Consumed, may be negative
	// Defined is false when no events were eligible.
	Defined bool `json:"defined"`
}

// NewBudget computes an error budget. With very few eligible events the
// budget is a fraction of one event, so a single failure exhausts it many
// times over; Consumed is reported as computed and the caller presents it
// with its Eligible count.
func NewBudget(target float64, eligible, bad uint64) Budget {
	b := Budget{Target: target, Eligible: eligible, Bad: bad}
	if eligible == 0 {
		return b
	}
	b.Defined = true
	b.Allowed = Round((1 - target) * float64(eligible))
	if b.Allowed > 0 {
		b.Consumed = Round(float64(bad) / ((1 - target) * float64(eligible)))
	}
	b.Remains = Round(1 - b.Consumed)
	return b
}

// BurnRate is the observed bad ratio divided by the allowed bad ratio
// (1 - target). A burn rate of 1 spends the budget exactly over the
// window; 14.4 over one hour spends 2% of a 30-day budget.
func BurnRate(target float64, eligible, bad uint64) float64 {
	if eligible == 0 || target >= 1 {
		return 0
	}
	return Round((float64(bad) / float64(eligible)) / (1 - target))
}

// ReliableQuantile reports whether a nearest-rank quantile q (permille) is
// supported by at least 10 observations above it: n * (1 - q) >= 10. Below
// that, the "p99" of a small sample is essentially its maximum.
func ReliableQuantile(n, permille uint64) bool {
	return n*(1000-permille) >= 10*1000
}
