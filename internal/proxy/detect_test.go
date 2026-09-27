package proxy

import (
	"strings"
	"testing"
)

// rep repeats s n times.
func rep(s string, n int) string {
	return strings.Repeat(s, n)
}

// distinctRunes returns a string of n runes that are all different, so no
// substring repeats within it.
func distinctRunes(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(rune(0x4e00 + i)) // CJK block: unique, multibyte
	}
	return b.String()
}

var defaultParams = Params{MinCount: 4, MinLen: 12, MaxLen: 200, MaxGap: 0}

func TestExactlyMinCountBackToBack(t *testing.T) {
	d := NewDetector(defaultParams)
	s := "abcdabcdabcd" // 12 runes
	if len([]rune(s)) != 12 {
		t.Fatalf("span is %d runes, want 12", len([]rune(s)))
	}
	// Feed across two fragments to prove detection works across Feeds.
	r, ok := d.Feed(rep(s, 2))
	if ok {
		t.Fatalf("triggered after 2 occurrences: %+v", r)
	}
	r, ok = d.Feed(rep(s, 2))
	if !ok {
		t.Fatalf("no trigger after 4 back-to-back occurrences")
	}
	if r.SpanLen != 12 || r.Span != s || r.Count != 4 {
		t.Fatalf("got %+v, want SpanLen=12 Span=%q Count=4", r, s)
	}
}

func TestOnlyThreeOccurrences(t *testing.T) {
	d := NewDetector(defaultParams)
	_, ok := d.Feed(rep("abcdabcdabcd", 3))
	if ok {
		t.Fatalf("triggered with only 3 occurrences")
	}
}

func TestSpanBelowMinLen(t *testing.T) {
	d := NewDetector(defaultParams)
	_, ok := d.Feed(rep("abcdefghijk", 4)) // 11 runes
	if ok {
		t.Fatalf("triggered with 11-rune span (MinLen=12)")
	}
}

func TestSpanAboveMaxLen(t *testing.T) {
	d := NewDetector(defaultParams)
	s := distinctRunes(201) // 201 runes, no internal repetition
	_, ok := d.Feed(rep(s, 4))
	if ok {
		t.Fatalf("triggered with 201-rune span (MaxLen=200)")
	}
}

func TestGapAboveMaxGap(t *testing.T) {
	s := "abcdabcdabcd" // 12 runes
	gap := "12345"      // 5 runes between occurrences
	text := strings.Join([]string{s, s, s, s}, gap)

	d0 := NewDetector(defaultParams) // MaxGap=0
	if _, ok := d0.Feed(text); ok {
		t.Fatalf("MaxGap=0: triggered with 5-rune gaps")
	}

	d5 := NewDetector(Params{MinCount: 4, MinLen: 12, MaxLen: 200, MaxGap: 5})
	r, ok := d5.Feed(text)
	if !ok {
		t.Fatalf("MaxGap=5: no trigger with 5-rune gaps")
	}
	if r.SpanLen != 12 || r.Count != 4 {
		t.Fatalf("got %+v, want SpanLen=12 Count=4", r)
	}
}

func TestRepeatInsideSingleFragment(t *testing.T) {
	d := NewDetector(defaultParams)
	s := "X12chars1234" // 12 runes
	if len([]rune(s)) != 12 {
		t.Fatalf("span is %d runes, want 12", len([]rune(s)))
	}
	r, ok := d.Feed(rep(s, 4)) // all in one Feed call
	if !ok {
		t.Fatalf("no trigger for repeat entirely inside one fragment")
	}
	if r.SpanLen != 12 || r.Count != 4 {
		t.Fatalf("got %+v, want SpanLen=12 Count=4", r)
	}
}

func TestMultibyteSpan(t *testing.T) {
	d := NewDetector(defaultParams)
	s := "héllo wörld🌍" // 12 runes (é, ö, 🌍 are multibyte)
	if n := len([]rune(s)); n != 12 {
		t.Fatalf("span is %d runes, want 12", n)
	}
	r, ok := d.Feed(rep(s, 4))
	if !ok {
		t.Fatalf("no trigger for multibyte span")
	}
	if r.SpanLen != 12 || r.Span != s || r.Count != 4 {
		t.Fatalf("got %+v, want SpanLen=12 (runes) Span=%q Count=4", r, s)
	}
}

func TestIdempotentAfterTrigger(t *testing.T) {
	d := NewDetector(defaultParams)
	s := "abcdabcdabcd"
	r1, ok := d.Feed(rep(s, 4))
	if !ok {
		t.Fatalf("no initial trigger")
	}
	for i := 0; i < 5; i++ {
		r2, ok2 := d.Feed("more text here")
		if !ok2 {
			t.Fatalf("Feed %d after trigger: ok=false", i)
		}
		if r2 != r1 {
			t.Fatalf("Feed %d after trigger: got %+v, want %+v", i, r2, r1)
		}
	}
}

func TestVariedTextNoTrigger(t *testing.T) {
	d := NewDetector(defaultParams)
	text := distinctRunes(5000) // longer than the scan window
	for i := 0; i < len(text); i += 100 {
		end := i + 100
		if end > len(text) {
			end = len(text)
		}
		if _, ok := d.Feed(text[i:end]); ok {
			t.Fatalf("triggered on varied text")
		}
	}
}

func TestLargerSpanPreferred(t *testing.T) {
	// S24 = A12+B12; text = (A12 B12) x 4.
	// The 24-rune span S24 occurs 4x back-to-back (gap 0).
	// The 12-rune span A12 occurs 4x with 12-rune gaps, which chains
	// only when MaxGap >= 12. With MaxGap=12 both trigger; the larger
	// span (24) must be reported.
	a := "abcdefghijk1" // 12 runes
	b := "mnopqrstuvwx" // 12 runes
	if len([]rune(a)) != 12 || len([]rune(b)) != 12 {
		t.Fatalf("helper spans are %d/%d runes, want 12/12", len([]rune(a)), len([]rune(b)))
	}
	text := rep(a+b, 4)
	d := NewDetector(Params{MinCount: 4, MinLen: 12, MaxLen: 200, MaxGap: 12})
	r, ok := d.Feed(text)
	if !ok {
		t.Fatalf("no trigger")
	}
	if r.SpanLen != 24 || r.Span != a+b || r.Count != 4 {
		t.Fatalf("got %+v, want SpanLen=24 Span=%q Count=4", r, a+b)
	}
}

func TestChainBreaksOnLargeGap(t *testing.T) {
	d := NewDetector(defaultParams)
	s := "abcdabcdabcd"                    // 12 runes
	filler := distinctRunes(100)           // no overlap with s
	text := rep(s, 2) + filler + rep(s, 2) // 4 occurrences, one 100-rune gap
	if _, ok := d.Feed(text); ok {
		t.Fatalf("triggered despite a gap > MaxGap in the chain")
	}
}
