package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestReplyBuilders pins the three parameterized rows of the contract
// table (they are the only replies that are not constants).
func TestReplyBuilders(t *testing.T) {
	if got, want := RplBanner("mx.example"), "220 mx.example ESMTP\r\n"; got != want {
		t.Errorf("RplBanner = %q, want %q", got, want)
	}
	if got, want := RplHelo("mx.example"), "250 mx.example\r\n"; got != want {
		t.Errorf("RplHelo = %q, want %q", got, want)
	}

	got := RplEhlo("mx.example", []string{"PIPELINING", "SIZE 10485760"})
	want := "250-mx.example\r\n250-PIPELINING\r\n250 SIZE 10485760\r\n"
	if got != want {
		t.Errorf("RplEhlo = %q, want %q", got, want)
	}
	if got, want := RplEhlo("mx.example", nil), "250 mx.example\r\n"; got != want {
		t.Errorf("RplEhlo with no capabilities = %q, want %q", got, want)
	}
}

// TestAuthReplies validates that each AUTH reply constant has the correct
// format: starts with a 3-digit code followed by a space, and ends with \r\n.
func TestAuthReplies(t *testing.T) {
	tests := []struct {
		name string
		got  string
		code string
	}{
		{"RplAuthOK", RplAuthOK, "235"},
		{"RplAuthContinue", RplAuthContinue, "334"},
		{"RplAuthMalformed", RplAuthMalformed, "501"},
		{"RplAuthCancelled", RplAuthCancelled, "501"},
		{"RplAuthMechanism", RplAuthMechanism, "504"},
		{"RplAuthRequired", RplAuthRequired, "530"},
		{"RplAuthFailed", RplAuthFailed, "535"},
		{"RplAuthEncryption", RplAuthEncryption, "538"},
		{"RplAuthTooMany", RplAuthTooMany, "421"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Check starts with code and space
			prefix := tt.code + " "
			if !strings.HasPrefix(tt.got, prefix) {
				t.Errorf("%s: want to start with %q, but got %q", tt.name, prefix, tt.got)
			}
			// Check ends with \r\n
			if !strings.HasSuffix(tt.got, "\r\n") {
				t.Errorf("%s: want to end with \\r\\n, but got %q", tt.name, tt.got)
			}
		})
	}
}

// TestNoReplyLiteralsOutsideReplies is decision D8's audit: replies.go is
// the closed enum of everything Bifrost says for itself, so an SMTP reply
// literal anywhere else in the package would be an unaudited synthesized
// reply — exactly what requirement R4 forbids. Test files are exempt:
// they assert the contract table as literals on purpose.
func TestNoReplyLiteralsOutsideReplies(t *testing.T) {
	// Unanchored on purpose: a reply literal assembled by concatenation
	// ("500 " + code) hides in the middle of a string just as well.
	replyLike := regexp.MustCompile(`[2-5][0-9][0-9][ -]`)

	// Recursive: later epics add subpackages under internal/proxy
	// (the relay engine), and the enum is closed for those too.
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == "replies.go" {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if replyLike.MatchString(val) {
				t.Errorf("%s: SMTP reply literal %q outside replies.go",
					fset.Position(lit.Pos()), val)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk package tree: %v", err)
	}
}
