package backend

import (
	"strconv"
	"strings"
)

// CapSet is a backend's parsed EHLO capability set: capability name
// (uppercased) to its parameter text exactly as advertised, verbatim —
// empty for a bare "NAME" line with no parameters.
type CapSet map[string]string

// Has reports whether name was advertised, case-insensitively.
func (c CapSet) Has(name string) bool {
	_, ok := c[strings.ToUpper(name)]
	return ok
}

// HasAuthPlain reports whether the backend's AUTH line lists PLAIN among
// its space-separated mechanisms, case-insensitively (e.g. "AUTH LOGIN
// PLAIN"). A missing AUTH capability entirely counts as false.
func (c CapSet) HasAuthPlain() bool {
	for _, mech := range strings.Fields(c["AUTH"]) {
		if strings.EqualFold(mech, "PLAIN") {
			return true
		}
	}
	return false
}

// Size reports the backend's advertised SIZE limit in bytes and whether
// it is bounded. A missing SIZE capability, a bare "SIZE" line, or "SIZE
// 0" all mean unlimited (RFC 1870): bounded is false in every one of
// those cases, since there is then no limit for a caller to compare
// against.
func (c CapSet) Size() (limit int64, bounded bool) {
	v, ok := c["SIZE"]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n == 0 {
		return 0, false
	}
	return n, true
}

// parseCaps builds a CapSet from every line of an EHLO reply, verbatim
// (terminator included). The first line is always the greeting/domain
// text per RFC 5321 4.1.1.1, not a capability, and is skipped. Each
// remaining line's text splits into "NAME" or "NAME value" on the first
// space; names fold to uppercase, values stay verbatim.
func parseCaps(lines [][]byte) CapSet {
	caps := make(CapSet, len(lines))
	for _, line := range lines[min(1, len(lines)):] {
		name, value := splitCapLine(replyText(line))
		if name == "" {
			continue
		}
		caps[strings.ToUpper(name)] = value
	}
	return caps
}

// replyText strips a verbatim reply line ("NNN-text\r\n", "NNN text\r\n",
// or "NNN\r\n") down to just its text, CRLF and code-plus-separator
// removed. Callers only ever pass lines smtpwire.ReplyReader already
// validated, so the CRLF and 3-digit code are guaranteed present.
func replyText(line []byte) []byte {
	body := line[:len(line)-2] // CRLF guaranteed by ReplyReader
	if len(body) <= 3 {
		return nil
	}
	return body[4:] // 3-digit code + one separator byte (' ' or '-')
}

// splitCapLine splits one EHLO extension line's text into its capability
// name and parameter, e.g. "SIZE 10485760" -> ("SIZE", "10485760") and
// "PIPELINING" -> ("PIPELINING", "").
func splitCapLine(text []byte) (name, value string) {
	s := strings.TrimSpace(string(text))
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i+1:])
}
