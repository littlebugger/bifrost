package config

import (
	"path/filepath"
	"testing"
)

// TestExamplesLoad proves every file under examples/ is a real, load-valid
// config -- not a sketch an operator has to hand-edit before it parses.
// examples/bifrost.hcl keeps its own detailed assertions in load_test.go;
// this only asserts zero error diagnostics (warnings, e.g. RFC-floor
// timeouts or an absent admin block, are fine).
func TestExamplesLoad(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.hcl")
	if err != nil {
		t.Fatalf("glob examples/*.hcl: %s", err)
	}
	if len(files) < 5 {
		t.Fatalf("found %d examples/*.hcl files, want at least 5 (bifrost.hcl + one per feature): %v", len(files), files)
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			_, diags := Load(path)
			if errs := diags.Errs(); len(errs) > 0 {
				t.Fatalf("Load(%s) returned %d error diagnostic(s): %v", path, len(errs), errs)
			}
		})
	}
}
