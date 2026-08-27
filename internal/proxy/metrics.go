package proxy

import "strconv"

// Metrics receives Bifrost's traffic-plane observability events — the
// Prometheus counters/gauges epic-09 defines (PROJECT.md's Produces
// block). It is a plain interface, not internal/metrics itself, for the
// same closure-inversion reason as HealthSignals/PickFunc/LeaseFunc:
// internal/proxy must never import internal/metrics. internal/metrics'
// concrete Metrics type satisfies this structurally, with no import
// needed on its side either.
//
// A nil Metrics is a no-op (noMetrics{}), exactly like a nil
// HealthSignals/LeaseFunc.
type Metrics interface {
	// SessionStarted/SessionEnded bracket one accepted client connection
	// that became a Session (bifrost_sessions_active/_total).
	SessionStarted()
	SessionEnded()

	// BackendDial reports one backend.Dial attempt and whether it
	// produced a usable leg (bifrost_backend_dials_total).
	BackendDial(server string, ok bool)

	// Transaction reports one concluded mail transaction: pool/server
	// are best-effort ("" when nothing ever attached, e.g. every
	// candidate failed to dial) and verdictClass is one of
	// "2xx"/"4xx"/"5xx" for a relayed backend verdict or
	// "synth_421"/"synth_451" for one Bifrost synthesized
	// (bifrost_transactions_total).
	Transaction(pool, server, verdictClass string)

	// SynthesizedReply reports one reply Bifrost generated itself rather
	// than relaying a backend's, keyed by its code and enhanced status
	// (e.g. "451 4.4.1") (bifrost_synthesized_replies_total).
	SynthesizedReply(codeEnhanced string)

	// RelayBytes reports n verbatim bytes moved in direction
	// ("to_backend" | "to_client") (bifrost_relay_bytes_total).
	RelayBytes(direction string, n int)

	// DuplicateRisk reports PROJECT.md's duplicate-delivery window: a
	// backend died after the final dot but before a verdict
	// (bifrost_duplicate_risk_total).
	DuplicateRisk()
}

// Byte-relay direction labels (bifrost_relay_bytes_total{direction}).
const (
	dirToBackend = "to_backend"
	dirToClient  = "to_client"
)

// noMetrics is the do-nothing Metrics used when nothing was wired in —
// mirrors noSignals (relay.go).
type noMetrics struct{}

func (noMetrics) SessionStarted()                    {}
func (noMetrics) SessionEnded()                      {}
func (noMetrics) BackendDial(string, bool)           {}
func (noMetrics) Transaction(string, string, string) {}
func (noMetrics) SynthesizedReply(string)            {}
func (noMetrics) RelayBytes(string, int)             {}
func (noMetrics) DuplicateRisk()                     {}

// firstMetrics returns overrides[0], or noMetrics{} if empty — the
// optional-trailing-argument shape NewRelay/Serve use so every pre-
// epic-09 call site keeps compiling unchanged. More than one argument
// is a caller bug (there is only ever one Metrics to wire in), and a
// nil first argument followed by a real one would otherwise silently
// discard the real one instead of using it — both panic rather than
// guess.
func firstMetrics(overrides []Metrics) Metrics {
	if len(overrides) > 1 {
		panic("proxy: at most one optional override")
	}
	if len(overrides) == 1 && overrides[0] != nil {
		return overrides[0]
	}
	return noMetrics{}
}

// codeEnhanced extracts a synthesized reply constant's code and enhanced
// status, e.g. "451 4.4.1 No backend available...\r\n" -> "451 4.4.1".
// Every constant in replies.go follows exactly this "CODE X.Y.Z text"
// shape.
func codeEnhanced(reply string) string {
	fields := 0
	for i := 0; i < len(reply); i++ {
		if reply[i] == ' ' {
			fields++
			if fields == 2 {
				return reply[:i]
			}
		}
	}
	return reply
}

// codeOf parses a synthesized reply constant's leading 3-digit code.
func codeOf(reply string) int {
	if len(reply) < 3 {
		return 0
	}
	n, err := strconv.Atoi(reply[:3])
	if err != nil {
		return 0
	}
	return n
}

// verdictClass turns a reply's leading digit into the metric's
// verdict_class label: a relayed backend reply classes by its first
// digit (2xx/4xx/5xx); a synthesized one names its own code instead
// (synth_421/synth_451), since PROJECT.md's contract cares about the
// difference between "the backend really said 451" and "Bifrost made
// this up".
func verdictClass(code int, synthesized bool) string {
	if synthesized {
		return "synth_" + strconv.Itoa(code)
	}
	switch code / 100 {
	case 2:
		return "2xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	default:
		return strconv.Itoa(code/100) + "xx"
	}
}
