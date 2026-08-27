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
