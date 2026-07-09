package benchcmp

import (
	"math"
	"slices"
)

// Median returns the middle observation (the mean of the two middle ones for an
// even count). The median rather than the mean, because one benchmark iteration
// that collided with a GC cycle or a background compile skews a mean and leaves
// a median alone — and those collisions are the norm, not the exception.
func Median(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// Spread reports the half-range as a percentage of the median: the "± 2%" in the
// report. It is a description of the noise, not a confidence interval — a wide
// spread means the number below it should not be trusted, whatever its p-value.
func Spread(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	lo, hi := slices.Min(values), slices.Max(values)
	median := Median(values)
	if median == 0 {
		return 0
	}
	return (hi - lo) / 2 / math.Abs(median) * 100
}

// exactLimit bounds the exact test. The exact distribution costs
// O(n1 * n2 * n1*n2) time and memory; at 20 vs 20 that is about a megabyte and a
// millisecond, and beyond it the normal approximation is excellent anyway
// (the U statistic converges quickly — n > 8 per side is the usual rule).
const exactLimit = 20

// MannWhitneyU returns the two-sided p-value for the null hypothesis that a and
// b are drawn from the same distribution, and the name of the method used.
//
// The test ranks the pooled observations and asks how improbable it is that one
// sample's ranks land as low as they did. It assumes nothing about the shape of
// the distribution — which is the whole point, since benchmark timings are
// bounded below and have a long tail, and so are conspicuously not normal.
//
// A p-value is not a probability that the optimization worked. It is the
// probability of seeing a difference this large if it did not.
func MannWhitneyU(a, b []float64) (p float64, method string) {
	n1, n2 := len(a), len(b)
	if n1 == 0 || n2 == 0 {
		return 1, "none"
	}

	ranks, tieTerm := rankPooled(a, b)

	var r1 float64
	for i := 0; i < n1; i++ {
		r1 += ranks[i]
	}
	u1 := r1 - float64(n1*(n1+1))/2
	u := math.Min(u1, float64(n1*n2)-u1)

	if tieTerm == 0 && n1 <= exactLimit && n2 <= exactLimit {
		return exactP(n1, n2, u), "exact"
	}
	return normalP(n1, n2, u, tieTerm), "normal"
}

// rankPooled assigns 1..N ranks over the concatenation a++b, averaging the ranks
// within each group of equal values. It also returns Σ(t³−t) over tie groups,
// which is the correction the normal approximation needs: ties shrink the
// variance of U, and ignoring them makes the test anticonservative.
//
// Ties are not a corner case here. Two benchmark runs reporting "0 allocs/op"
// ten times each are all ties, and that comparison must still be sound.
func rankPooled(a, b []float64) (ranks []float64, tieTerm float64) {
	n := len(a) + len(b)
	pooled := make([]float64, 0, n)
	pooled = append(pooled, a...)
	pooled = append(pooled, b...)

	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(i, j int) int {
		switch {
		case pooled[i] < pooled[j]:
			return -1
		case pooled[i] > pooled[j]:
			return 1
		default:
			return 0
		}
	})

	ranks = make([]float64, n)
	for i := 0; i < n; {
		j := i
		for j+1 < n && pooled[order[j+1]] == pooled[order[i]] {
			j++
		}
		// Ranks are 1-based; the group spans positions i..j inclusive.
		avg := float64(i+j+2) / 2
		for k := i; k <= j; k++ {
			ranks[order[k]] = avg
		}
		if t := float64(j - i + 1); t > 1 {
			tieTerm += t*t*t - t
		}
		i = j + 1
	}
	return ranks, tieTerm
}

// exactP computes the two-sided p-value from the exact null distribution of U.
//
// count[i][j][k] is the number of ways to interleave i and j observations so
// that the U statistic equals k. The recurrence considers the largest pooled
// observation: it belongs either to the first sample (contributing j inversions)
// or to the second (contributing none).
func exactP(n1, n2 int, u float64) float64 {
	uMax := n1 * n2
	prev := make([][]float64, n2+1) // count[i-1][j][*]
	cur := make([][]float64, n2+1)
	for j := range prev {
		prev[j] = make([]float64, uMax+1)
		cur[j] = make([]float64, uMax+1)
	}
	// i = 0: one arrangement, U = 0, for every j.
	for j := 0; j <= n2; j++ {
		prev[j][0] = 1
	}

	for i := 1; i <= n1; i++ {
		clear(cur[0])
		cur[0][0] = 1
		for j := 1; j <= n2; j++ {
			for k := 0; k <= uMax; k++ {
				v := cur[j-1][k] // the largest element came from sample 2
				if k-j >= 0 {
					v += prev[j][k-j] // ...or from sample 1, passing j of sample 2
				}
				cur[j][k] = v
			}
		}
		prev, cur = cur, prev
	}

	dist := prev[n2]
	var total, lower float64
	for k := 0; k <= uMax; k++ {
		total += dist[k]
		if float64(k) <= u {
			lower += dist[k]
		}
	}
	if total == 0 {
		return 1
	}
	// U was taken as min(U1, U2), so its distribution is folded and the lower
	// tail is exactly half of the two-sided rejection region.
	return math.Min(1, 2*lower/total)
}

// normalP is the large-sample approximation, with a continuity correction and
// the tie correction to the variance.
func normalP(n1, n2 int, u, tieTerm float64) float64 {
	n := float64(n1 + n2)
	mu := float64(n1*n2) / 2

	variance := float64(n1*n2) / 12 * ((n + 1) - tieTerm/(n*(n-1)))
	if variance <= 0 {
		// Every observation is identical: U is deterministic, nothing to reject.
		return 1
	}
	sigma := math.Sqrt(variance)

	// u <= mu because u is the folded statistic; the correction moves it toward
	// the mean, which is the conservative direction.
	z := (u - mu + 0.5) / sigma
	if z > 0 {
		z = 0
	}
	return math.Min(1, math.Erfc(math.Abs(z)/math.Sqrt2))
}
