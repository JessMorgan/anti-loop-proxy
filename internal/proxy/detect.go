// Package proxy implements stream-level content processing helpers.
package proxy

import "sort"

// Params holds detection thresholds.
type Params struct {
	MinCount int // min number of identical span occurrences to trigger (>= 2)
	MinLen   int // min span length in runes (>= 1)
	MaxLen   int // max span length in runes (>= MinLen)
	MaxGap   int // max non-duplicated runes allowed between consecutive occurrences (>= 0)
}

// Result is returned when a duplication is detected.
type Result struct {
	SpanLen int    // length of the repeated span in runes
	Span    string // the repeated span (runes)
	Count   int    // number of occurrences counted (including the most recent one)
}

// Detector tracks emitted text content and detects repeated spans.
// It is safe for sequential use per stream (not goroutine-safe).
type Detector struct {
	buf       []rune
	params    Params
	triggered bool
	result    Result
}

// NewDetector creates a Detector with the given thresholds.
func NewDetector(p Params) *Detector {
	return &Detector{params: p}
}

// Feed appends a text fragment and reports a trigger if the thresholds are
// now met. Once triggered, all subsequent Feed calls return the same
// Result (idempotent, no re-scan).
func (d *Detector) Feed(fragment string) (Result, bool) {
	if d.triggered {
		return d.result, true
	}
	d.buf = append(d.buf, []rune(fragment)...)
	r, ok := d.scan()
	if ok {
		d.triggered = true
		d.result = r
	}
	return r, ok
}

// scan checks the buffer for a triggering repeated span and returns the
// largest span length that triggers.
func (d *Detector) scan() (Result, bool) {
	p := d.params
	n := len(d.buf)
	if n < p.MinLen || p.MinCount < 2 {
		return Result{}, false
	}
	maxL := p.MaxLen
	if n < maxL {
		maxL = n
	}
	if maxL < p.MinLen {
		return Result{}, false
	}

	// The triggering chain can only live in the last W runes: it holds
	// MinCount occurrences of a span of at most MaxLen separated by gaps
	// of at most MaxGap, plus the span itself.
	window := p.MaxLen*(p.MinCount+1) + p.MaxGap*p.MinCount + p.MaxLen
	start := n - window
	if start < 0 {
		start = 0
	}

	// Iterate span lengths from largest to smallest so the largest
	// triggering span is reported.
	for L := maxL; L >= p.MinLen; L-- {
		s := d.buf[n-L:]

		// Collect occurrence start positions of s within the window.
		positions := make([]int, 0, p.MinCount)
		for i := start; i+L <= n; i++ {
			if d.buf[i] != s[0] {
				continue
			}
			if runesEqual(d.buf[i:i+L], s) {
				positions = append(positions, i)
			}
		}
		// The final occurrence must end at len(buf).
		if len(positions) == 0 || positions[len(positions)-1] != n-L {
			continue
		}

		// Walk the occurrences backwards from the last one. Each
		// predecessor must be non-overlapping (end at or before the
		// later start) and within MaxGap, so overlapping occurrences
		// are skipped, not chained.
		chain := 1
		cur := len(positions) - 1
		for chain < p.MinCount {
			// Predecessor start must lie in [cur-L-MaxGap, cur-L].
			hi := positions[cur] - L
			lo := hi - p.MaxGap
			j := sort.Search(len(positions), func(k int) bool {
				return positions[k] > hi
			}) - 1
			if j < 0 || positions[j] < lo {
				break
			}
			chain++
			cur = j
		}
		if chain >= p.MinCount {
			return Result{SpanLen: L, Span: string(s), Count: chain}, true
		}
	}
	return Result{}, false
}

// runesEqual reports whether a and b contain the same runes.
func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
