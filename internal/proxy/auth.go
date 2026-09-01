package proxy

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"

	"github.com/littlebugger/bifrost/internal/config"
)

// parsePlain decodes a PLAIN initial-response/continuation payload
// (RFC 4616): base64 of authzid \x00 authcid \x00 passwd.
func parsePlain(b64 []byte) (authzid, authcid, password string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b64)))
	if err != nil {
		return "", "", "", false
	}
	parts := bytes.Split(raw, []byte{0})
	if len(parts) != 3 {
		return "", "", "", false
	}
	return string(parts[0]), string(parts[1]), string(parts[2]), true
}

// verifyPlain reports whether (authcid, password) matches a configured
// user: sha256(salt || password) in constant time. Unknown users burn
// the same work against a dummy so timing does not reveal valid names.
func verifyPlain(users []config.AuthUser, authcid, password string) bool {
	// Dummy entry: unknown users cost the same hash+compare as known
	// ones, so response timing does not enumerate valid names.
	match := config.AuthUser{Salt: "-", HashedPassword: strings.Repeat("0", 64)}
	found := 0
	for i := range users {
		if users[i].Name == authcid {
			match, found = users[i], 1
		}
	}
	sum := sha256.Sum256([]byte(match.Salt + password))
	got := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(match.HashedPassword))&found == 1
}
