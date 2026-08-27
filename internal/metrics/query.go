package metrics

import dto "github.com/prometheus/client_model/go"

// This file is the read side of a Gather() result: small helpers for
// picking one series' value out of []*dto.MetricFamily by name and
// label filter. internal/admin's /stats endpoint uses these to reshape
// the registry into JSON; this package's own integration tests use them
// to assert on it.

// CounterValue returns the value of the first series in name matching
// every label in want (extra labels on the series are ignored), or 0 if
// none matches. want == nil matches an unlabeled series (or the first
// series found, for a labeled one).
func CounterValue(mfs []*dto.MetricFamily, name string, want map[string]string) float64 {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !labelsMatch(m, want) {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
		}
	}
	return 0
}

// GaugeValue is CounterValue for a gauge-typed family.
func GaugeValue(mfs []*dto.MetricFamily, name string, want map[string]string) float64 {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !labelsMatch(m, want) {
				continue
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue()
			}
		}
	}
	return 0
}

// SumCounter adds every series in name whose labels are a superset of
// want — e.g. summing bifrost_transactions_total across verdict_class
// for one {pool,server}.
func SumCounter(mfs []*dto.MetricFamily, name string, want map[string]string) float64 {
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m, want) {
				if c := m.GetCounter(); c != nil {
					total += c.GetValue()
				}
			}
		}
	}
	return total
}

// labelsMatch reports whether metric carries every (key, value) in want.
// A nil/empty want matches anything.
func labelsMatch(metric *dto.Metric, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	got := make(map[string]string, len(metric.GetLabel()))
	for _, lp := range metric.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
