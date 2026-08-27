package proxy

import "testing"

// TestFirstMetricsRejectsMultiple: more than one optional override is a
// caller bug (NewRelay/Serve's variadic Metrics is meant to carry at
// most one) — it must panic rather than silently pick one and drop the
// rest, which would be especially misleading if the dropped one were
// the real Metrics behind a nil placeholder.
func TestFirstMetricsRejectsMultiple(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("firstMetrics with 2 overrides: want a panic, got none")
		}
	}()
	firstMetrics([]Metrics{nil, noMetrics{}})
}

func TestFirstMetricsZeroOrOne(t *testing.T) {
	want := noMetrics{}
	if got := firstMetrics(nil); got != want {
		t.Fatalf("firstMetrics(nil) = %v, want noMetrics{}", got)
	}
	if got := firstMetrics([]Metrics{nil}); got != want {
		t.Fatalf("firstMetrics([nil]) = %v, want noMetrics{}", got)
	}
	m := noMetrics{}
	if got := firstMetrics([]Metrics{m}); got != Metrics(m) {
		t.Fatalf("firstMetrics([m]) = %v, want m", got)
	}
}
