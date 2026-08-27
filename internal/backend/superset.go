package backend

import (
	"fmt"
	"strconv"
	"strings"
)

// IncompatibleError reports that a backend's advertised capability set
// is not a superset of what the listener requires (PROJECT.md's
// capability policy). Missing holds each failing requirement verbatim,
// e.g. "8BITMIME" or "SIZE 10485760" (too small). The health prober
// (epic 06) turns this into the "incompatible" verdict.
type IncompatibleError struct {
	Missing []string
}

func (e *IncompatibleError) Error() string {
	return fmt.Sprintf("backend: incompatible, missing/insufficient: %s", strings.Join(e.Missing, ", "))
}

// checkSuperset verifies that caps is a superset of required, applying
// PROJECT.md's two carve-outs: STARTTLS is excluded entirely (it is
// client-leg-owned; backend TLS is governed solely by the pool's
// backend_tls mode, enforced by the handshake itself, not here), and
// SIZE compares numerically with a bare "SIZE" or "SIZE 0" meaning
// unlimited (RFC 1870), which satisfies any required value.
func checkSuperset(caps CapSet, required []string) error {
	var missing []string
	for _, req := range required {
		verb, arg, _ := strings.Cut(strings.ToUpper(strings.TrimSpace(req)), " ")
		switch verb {
		case "STARTTLS":
			continue
		case "SIZE":
			if !sizeSatisfies(caps, arg) {
				missing = append(missing, req)
			}
		default:
			if !caps.Has(verb) {
				missing = append(missing, req)
			}
		}
	}
	if len(missing) > 0 {
		return &IncompatibleError{Missing: missing}
	}
	return nil
}

// sizeSatisfies reports whether caps' advertised SIZE meets a required
// byte count given as text (e.g. "10485760"). Unlimited on either side —
// caps advertising a bare/zero SIZE, or a required value that itself
// parses to 0 or isn't a number — satisfies the comparison. A backend
// that omits the SIZE extension entirely is NOT unlimited: it never
// advertised any size promise, so it fails whenever a non-zero size is
// required. PROJECT.md's carve-out covers only bare SIZE / SIZE 0 (the
// extension present with no bound), never SIZE's absence.
func sizeSatisfies(caps CapSet, requiredArg string) bool {
	requiredN, err := strconv.ParseInt(strings.TrimSpace(requiredArg), 10, 64)
	if err != nil || requiredN == 0 {
		return true
	}
	if !caps.Has("SIZE") {
		return false
	}
	advertised, bounded := caps.Size()
	if !bounded {
		return true
	}
	return advertised >= requiredN
}
