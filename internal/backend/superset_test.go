package backend

import (
	"crypto/tls"
	"errors"
	"testing"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

func TestSupersetOK(t *testing.T) {
	caps := CapSet{"PIPELINING": "", "8BITMIME": "", "SIZE": "20971520"}
	required := []string{"PIPELINING", "8BITMIME", "SIZE 10485760"}

	if err := checkSuperset(caps, required); err != nil {
		t.Fatalf("checkSuperset() = %v, want nil (20M advertised >= 10M required)", err)
	}
}

// TestSupersetMissingCapability is backend-missing-capability-marked-out:
// the backend never advertised 8BITMIME at all.
func TestSupersetMissingCapability(t *testing.T) {
	caps := CapSet{"PIPELINING": ""}
	required := []string{"PIPELINING", "8BITMIME"}

	err := checkSuperset(caps, required)
	var ierr *IncompatibleError
	if !errors.As(err, &ierr) {
		t.Fatalf("checkSuperset() = %v (%T), want *IncompatibleError", err, err)
	}
	if len(ierr.Missing) != 1 || ierr.Missing[0] != "8BITMIME" {
		t.Errorf("Missing = %v, want [\"8BITMIME\"]", ierr.Missing)
	}
}

// TestSupersetSizeTooSmall is advertised-size-le-min-backend-size: the
// backend's SIZE is smaller than what the listener requires.
func TestSupersetSizeTooSmall(t *testing.T) {
	caps := CapSet{"SIZE": "5242880"} // 5M
	required := []string{"SIZE 10485760"}

	err := checkSuperset(caps, required)
	var ierr *IncompatibleError
	if !errors.As(err, &ierr) {
		t.Fatalf("checkSuperset() = %v (%T), want *IncompatibleError", err, err)
	}
	if len(ierr.Missing) != 1 || ierr.Missing[0] != "SIZE 10485760" {
		t.Errorf("Missing = %v, want a SIZE-naming entry", ierr.Missing)
	}
}

// TestSupersetSizeAbsentFails: a backend that never advertises the SIZE
// extension at all is not the same as one that advertises it bare or as
// "SIZE 0" — it made no size promise, so it fails a non-zero requirement
// instead of passing as unlimited.
func TestSupersetSizeAbsentFails(t *testing.T) {
	caps := CapSet{"PIPELINING": ""} // no SIZE key at all
	required := []string{"SIZE 10485760"}

	err := checkSuperset(caps, required)
	var ierr *IncompatibleError
	if !errors.As(err, &ierr) {
		t.Fatalf("checkSuperset() = %v (%T), want *IncompatibleError (SIZE absent is not unlimited)", err, err)
	}
	if len(ierr.Missing) != 1 || ierr.Missing[0] != "SIZE 10485760" {
		t.Errorf("Missing = %v, want a SIZE-naming entry", ierr.Missing)
	}
}

// TestSupersetSizeZeroUnlimited: RFC 1870, "SIZE 0" on the wire means
// unlimited and satisfies any required value.
func TestSupersetSizeZeroUnlimited(t *testing.T) {
	caps := CapSet{"SIZE": "0"}
	required := []string{"SIZE 10485760"}

	if err := checkSuperset(caps, required); err != nil {
		t.Fatalf("checkSuperset() = %v, want nil (SIZE 0 is unlimited)", err)
	}
}

// TestSupersetSizeBareKeyword: a bare "SIZE" (no value at all) is the
// other RFC 1870 unlimited spelling.
func TestSupersetSizeBareKeyword(t *testing.T) {
	caps := CapSet{"SIZE": ""}
	required := []string{"SIZE 10485760"}

	if err := checkSuperset(caps, required); err != nil {
		t.Fatalf("checkSuperset() = %v, want nil (bare SIZE is unlimited)", err)
	}
}

// TestSupersetIgnoresStarttls: STARTTLS is excluded from the superset
// comparison entirely — it is client-leg-owned, and backend TLS is
// enforced by the handshake itself (pool backend_tls), not by this
// check. A plaintext backend that never advertises STARTTLS is still
// compatible with a required set that lists it.
func TestSupersetIgnoresStarttls(t *testing.T) {
	caps := CapSet{"PIPELINING": ""} // no STARTTLS advertised
	required := []string{"PIPELINING", "STARTTLS"}

	if err := checkSuperset(caps, required); err != nil {
		t.Fatalf("checkSuperset() = %v, want nil (STARTTLS ignored in the comparison)", err)
	}
}

// TestDialIncompatibleCapabilities is the end-to-end counterpart to the
// checkSuperset unit tests above: a real Dial against a real fake,
// proving the check is actually wired in and IncompatibleError comes
// back errors.As-able, naming the missing capability.
func TestDialIncompatibleCapabilities(t *testing.T) {
	srv := fakesmtp.Start(t, fakesmtp.Script{Caps: []string{"PIPELINING"}})

	_, err := dialTest(t, srv.Addr(), Opts{
		EhloName:     "client.example",
		TLSMode:      "none",
		RequiredCaps: []string{"PIPELINING", "8BITMIME"},
		Timeouts:     testTimeouts(),
	})
	var ierr *IncompatibleError
	if !errors.As(err, &ierr) {
		t.Fatalf("Dial err = %v (%T), want *IncompatibleError", err, err)
	}
	if len(ierr.Missing) != 1 || ierr.Missing[0] != "8BITMIME" {
		t.Errorf("Missing = %v, want [\"8BITMIME\"]", ierr.Missing)
	}
}

// TestDialIncompatibleCapabilitiesPostTLS proves the superset check runs
// against the POST-TLS capability set when TLSMode is starttls, not the
// pre-TLS one: the fake is scripted to advertise 8BITMIME only on its
// FIRST (pre-TLS) EHLO and drop it on the SECOND (post-TLS, after the
// mandatory re-EHLO). If Dial checked the pre-TLS set, this would wrongly
// succeed.
func TestDialIncompatibleCapabilitiesPostTLS(t *testing.T) {
	cfg := fakesmtp.TestCert(t)
	srv := fakesmtp.Start(t, fakesmtp.Script{
		TLS: cfg,
		OnEHLO: []fakesmtp.Step{
			// Custom Reply text bypasses fakesmtp's auto-appended STARTTLS,
			// so it must be listed explicitly here for the handshake's own
			// STARTTLS to proceed.
			{Reply: "250-fakesmtp\r\n250-PIPELINING\r\n250-8BITMIME\r\n250 STARTTLS"}, // pre-TLS: satisfies RequiredCaps
			{Reply: "250-fakesmtp\r\n250 PIPELINING"},                                 // post-TLS: 8BITMIME dropped
		},
	})

	_, err := dialTest(t, srv.Addr(), Opts{
		EhloName:     "client.example",
		TLSMode:      "starttls",
		TLSConfig:    &tls.Config{ServerName: "127.0.0.1"}, // starttls mode never verifies; see startTLS
		RequiredCaps: []string{"PIPELINING", "8BITMIME"},
		Timeouts:     testTimeouts(),
	})
	var ierr *IncompatibleError
	if !errors.As(err, &ierr) {
		t.Fatalf("Dial err = %v (%T), want *IncompatibleError (the post-TLS EHLO dropped 8BITMIME)", err, err)
	}
	if len(ierr.Missing) != 1 || ierr.Missing[0] != "8BITMIME" {
		t.Errorf("Missing = %v, want [\"8BITMIME\"] (proves the POST-TLS set, not the pre-TLS one that had it, is what gets checked)", ierr.Missing)
	}
}
