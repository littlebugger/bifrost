// Command fakesmtp is a standalone, scriptable-at-startup-only fake SMTP
// backend: a default-250 (accept everything) server whose advertised
// EHLO capabilities and per-reply delay are set from flags. It exists so
// load runs (cmd/loadgen, scripts/load_smoke.sh) have a real out-of-
// process backend to point at; anything needing a scripted Script
// beyond Caps/Delay (verdict sequences, drops, TLS) stays an in-process
// Go test against internal/fakesmtp directly -- a JSON script format is
// unimplementable anyway, since Script.TLS is a *tls.Config and does not
// marshal.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/littlebugger/bifrost/internal/fakesmtp"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run implements the CLI over explicit stdout/stderr writers, the same
// shape cmd/bifrost and cmd/loadgen use. It blocks until SIGTERM/SIGINT,
// like any other server process.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fakesmtp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:0", "address to listen on")
	caps := fs.String("caps", "PIPELINING,8BITMIME", "comma-separated EHLO capabilities to advertise")
	delay := fs.Duration("delay", 0, "delay applied before every reply (banner included)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	srv, err := fakesmtp.StartAddr(*listen, script(capList(*caps), *delay))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fakesmtp: %v\n", err)
		return 1
	}
	defer srv.Stop()
	_, _ = fmt.Fprintf(stdout, "fakesmtp listening on %s\n", srv.Addr())

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	<-sigc
	return 0
}

// capList splits a comma-separated capability list, trimming whitespace
// and dropping empty entries (so "" yields no capabilities at all,
// rather than one blank one).
func capList(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// script builds the fixed Script every connection to this binary gets:
// the given capabilities, and delay applied uniformly before every
// reply. Each per-verb queue is one repeating Step (fakesmtp repeats a
// queue's last entry forever once it's short), with an empty Reply so
// every verb still gets its normal default text -- only the delay is
// overridden, uniformly, verb by verb.
func script(caps []string, delay time.Duration) fakesmtp.Script {
	step := []fakesmtp.Step{{Delay: delay}}
	return fakesmtp.Script{
		Banner: fakesmtp.Step{Delay: delay},
		Caps:   caps,
		OnEHLO: step,
		OnMAIL: step,
		OnRCPT: step,
		OnDATA: step,
		OnEOD:  step,
		OnRSET: step,
		OnQUIT: step,
	}
}
