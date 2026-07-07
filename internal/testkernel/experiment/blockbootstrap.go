// blockbootstrap.go — First Instruments Module D5: circular block-bootstrap
// for serially-dependent REAL consecutive-interval data (blind-review-4
// Finding C).
//
// The real M11r estimator is built from n CONSECUTIVE inter-consolidation
// intervals over ONE boot (the frozen replicate protocol — see runner.go),
// which are serially dependent (each interval's phase depends on the
// previous tick's jitter). All bootstrap CIs on real consecutive-interval
// data use a circular/moving block-bootstrap with block length
// l=ceil(sqrt(n)) (resample BLOCKS, not individual intervals). The
// config-independent synthetic nulls in calibration.go are i.i.d. by
// construction and use ordinary (block length 1) bootstrap instead — see
// BootstrapCIUpper.
package experiment

import (
	"math"
	"math/rand"
	"sort"
)

// BlockLength returns the frozen circular block-bootstrap block length
// l=ceil(sqrt(n)) for n consecutive real intervals (PREREG §6.2 / Finding
// C). i.i.d. synthetic nulls use l=1 instead (ordinary bootstrap).
func BlockLength(n int) int {
	if n <= 0 {
		return 1
	}
	l := int(math.Ceil(math.Sqrt(float64(n))))
	if l < 1 {
		l = 1
	}
	return l
}

// circularBlockResample draws one circular-block-bootstrap resample of
// length n from data (wrapping indices modulo len(data), so every block
// start is valid even near the end of the series — the "circular" part of
// circular block-bootstrap).
func circularBlockResample(rng *rand.Rand, data []float64, blockLen int) []float64 {
	n := len(data)
	if n == 0 {
		return nil
	}
	out := make([]float64, 0, n)
	for len(out) < n {
		start := rng.Intn(n)
		for i := 0; i < blockLen && len(out) < n; i++ {
			out = append(out, data[(start+i)%n])
		}
	}
	return out
}

// BlockBootstrapMeanCI computes the bootstrapped `level` (two-sided) CI of
// the MEAN of real, serially-dependent consecutive-interval data, via the
// circular block-bootstrap (block length l=ceil(sqrt(n)), B resamples).
// Used for SE_hat's real-data counterpart and for the real M11r cadence
// estimator's own CI (as opposed to the synthetic-null S-statistic CIs in
// calibration.go, which use ordinary i.i.d. bootstrap).
func BlockBootstrapMeanCI(data []float64, seed int64, level float64, b int) (upper, half float64) {
	n := len(data)
	if n == 0 {
		return math.NaN(), math.NaN()
	}
	blockLen := BlockLength(n)
	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, b)
	for i := 0; i < b; i++ {
		resample := circularBlockResample(rng, data, blockLen)
		means[i] = mean(resample)
	}
	sort.Float64s(means)
	alpha := (1.0 - level) / 2.0
	lo := percentile(means, alpha)
	hi := percentile(means, 1.0-alpha)
	return hi, (hi - lo) / 2.0
}
