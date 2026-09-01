package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/littlebugger/bifrost/internal/config"
)

func testAuthUser(name, salt, password string) config.AuthUser {
	sum := sha256.Sum256([]byte(salt + password))
	hashed := hex.EncodeToString(sum[:])
	return config.AuthUser{
		Name:           name,
		Salt:           salt,
		HashedPassword: hashed,
	}
}

func TestParsePlain(t *testing.T) {
	tests := []struct {
		name         string
		b64          string
		wantAuthzid  string
		wantAuthcid  string
		wantPassword string
		wantOk       bool
	}{
		{
			name:         "happy path: empty authzid",
			b64:          base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret")),
			wantAuthzid:  "",
			wantAuthcid:  "alice",
			wantPassword: "secret",
			wantOk:       true,
		},
		{
			name:         "nonempty authzid",
			b64:          base64.StdEncoding.EncodeToString([]byte("bob\x00alice\x00secret")),
			wantAuthzid:  "bob",
			wantAuthcid:  "alice",
			wantPassword: "secret",
			wantOk:       true,
		},
		{
			name:         "empty password",
			b64:          base64.StdEncoding.EncodeToString([]byte("\x00alice\x00")),
			wantAuthzid:  "",
			wantAuthcid:  "alice",
			wantPassword: "",
			wantOk:       true,
		},
		{
			name:   "bad base64",
			b64:    "!!!invalid!!!",
			wantOk: false,
		},
		{
			name:   "one NUL only",
			b64:    base64.StdEncoding.EncodeToString([]byte("alice\x00secret")),
			wantOk: false,
		},
		{
			name:   "three NULs",
			b64:    base64.StdEncoding.EncodeToString([]byte("bob\x00alice\x00secret\x00extra")),
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authzid, authcid, password, ok := parsePlain([]byte(tt.b64))
			if ok != tt.wantOk {
				t.Errorf("parsePlain ok = %v, want %v", ok, tt.wantOk)
			}
			if tt.wantOk {
				if authzid != tt.wantAuthzid {
					t.Errorf("parsePlain authzid = %q, want %q", authzid, tt.wantAuthzid)
				}
				if authcid != tt.wantAuthcid {
					t.Errorf("parsePlain authcid = %q, want %q", authcid, tt.wantAuthcid)
				}
				if password != tt.wantPassword {
					t.Errorf("parsePlain password = %q, want %q", password, tt.wantPassword)
				}
			}
		})
	}
}

func TestVerifyPlain(t *testing.T) {
	users := []config.AuthUser{
		testAuthUser("alice", "s1", "pw"),
		testAuthUser("bob", "s1", "different"),
	}

	tests := []struct {
		name      string
		authcid   string
		password  string
		wantMatch bool
	}{
		{
			name:      "correct creds",
			authcid:   "alice",
			password:  "pw",
			wantMatch: true,
		},
		{
			name:      "wrong password",
			authcid:   "alice",
			password:  "wrong",
			wantMatch: false,
		},
		{
			name:      "unknown user",
			authcid:   "charlie",
			password:  "pw",
			wantMatch: false,
		},
		{
			name:      "bob correct creds",
			authcid:   "bob",
			password:  "different",
			wantMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := verifyPlain(users, tt.authcid, tt.password)
			if got != tt.wantMatch {
				t.Errorf("verifyPlain = %v, want %v", got, tt.wantMatch)
			}
		})
	}

	// Hash case-insensitivity: config load normalizes hashes to lowercase (see
	// internal/config/load.go), so verifyPlain does literal byte comparison and
	// rejects uppercase hashes. This test documents that invariant.
	t.Run("uppercase hash rejected", func(t *testing.T) {
		correctHash := testAuthUser("dave", "s1", "secret").HashedPassword
		uppercaseHash := strings.ToUpper(correctHash)
		badUser := config.AuthUser{
			Name:           "dave",
			Salt:           "s1",
			HashedPassword: uppercaseHash,
		}
		got := verifyPlain([]config.AuthUser{badUser}, "dave", "secret")
		if got {
			t.Errorf("verifyPlain with uppercase hash = true, want false")
		}
	})
}
