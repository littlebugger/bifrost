package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCheckModeOK(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-c", "-f", "../../examples/bifrost.hcl"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(-c examples/bifrost.hcl) = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), "config OK")
	}
}

func TestCheckModeBad(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fixture := "../../internal/config/testdata/bad-weight.hcl"
	code := run([]string{"-c", "-f", fixture}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(-c %s) = %d, want 1; stdout=%q stderr=%q", fixture, code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "config OK") {
		t.Errorf("stdout = %q, want no \"config OK\" on a failing check", stdout.String())
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "bad-weight.hcl:") {
		t.Errorf("stderr = %q, want a diagnostic naming %s with a line number (file:line)", errOut, fixture)
	}
	if !strings.Contains(errOut, "Weight out of range") {
		t.Errorf("stderr = %q, want it to mention the specific violation", errOut)
	}
}
