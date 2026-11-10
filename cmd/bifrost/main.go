// Command bifrost is an SMTP-aware, cut-through load balancer.
// See PROJECT.md for the full design.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/littlebugger/bifrost/internal/config"
	"github.com/littlebugger/bifrost/internal/proxy"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// appHook, when non-nil, is invoked with the just-built *app right after
// newApp succeeds: main_test.go's only way to reach into the live
// listener sockets (serve itself never exposes app), mirroring wire.go's
// defaultLookup test seam. Nil in production. Like defaultLookup, it is
// process-global: no two tests that set it may run in parallel.
var appHook func(*app)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run implements the CLI over explicit stdout/stderr writers (rather than
// touching os.Stdout/os.Stderr or calling os.Exit itself) so tests can
// drive it in-process instead of exec'ing the built binary. It returns
// the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bifrost", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print version and exit")
	checkMode := fs.Bool("c", false, "check config and exit")
	configPath := fs.String("f", "", "path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "bifrost %s\n", version)
		return 0
	}

	if *checkMode {
		return runCheck(*configPath, stdout, stderr)
	}

	if *configPath == "" {
		_, _ = fmt.Fprintln(stderr, "bifrost: -f <path> is required")
		return 2
	}
	cfg, diags := config.Load(*configPath)
	for _, d := range diags {
		_, _ = fmt.Fprintln(stderr, d.Error())
	}
	if diags.HasErrors() {
		return 1
	}
	return serve(*configPath, cfg, stderr)
}

// serve runs the process until a shutdown signal: the SMTP accept loop,
// health checker, and admin plane as long-runners under one WaitGroup and
// one error channel — stdlib only, no errgroup (epic-00's dependency
// budget) — plus the signal loop, plus a conditional fourth long-runner
// (dns discovery) when the config declares any server_dns template.
// SIGHUP reloads the config file in place; SIGTERM/SIGINT run the drain
// sequence and exit 0.
//
// Two contexts, not one: sessionCtx is cancelled when the drain starts (it
// is what tells sessions the process is going away), rootCtx only once
// the drain is over — the admin plane has to keep answering /healthz 503
// for the whole lame-duck window, which a single context could not
// express.
func serve(cfgPath string, cfg *config.Config, stderr io.Writer) int {
	lg := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	a, err := newApp(cfgPath, cfg, lg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bifrost: %v\n", err)
		return 1
	}
	defer a.close()
	if appHook != nil {
		appHook(a)
	}

	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	sessionCtx, cancelSessions := context.WithCancel(rootCtx)
	defer cancelSessions()

	var wg sync.WaitGroup
	errc := make(chan error, len(a.smtpLns)+3) // every listener plus the 3 other long-runners: no sender ever blocks
	serveDone := make(chan struct{})
	start := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	// Each listener's proxy.Serve owns its own session WaitGroup, so its
	// return means nothing is in flight on that listener any more.
	// serveDone — the signal the drain sequence waits on — only closes
	// once ALL of them have returned, via the smtpWG + closer goroutine
	// below, not the first one back. A listener returning without a drain
	// in progress (sessionCtx still live) is caught per-listener, below,
	// via errc — not by serveDone, which N-1 still-healthy listeners would
	// keep from ever firing.
	var smtpWG sync.WaitGroup
	for _, nl := range a.smtpLns {
		smtpWG.Add(1)
		start("smtp listener "+nl.name, func() error {
			defer smtpWG.Done()
			err := proxy.Serve(sessionCtx, nl.ln, cfg, nl.lst, nl.tlsCfg, a.relay, lg, a.globalConns, a.metrics)
			if err == nil && sessionCtx.Err() == nil {
				err = errors.New("stopped unexpectedly: no drain in progress")
			}
			return err
		})
	}
	go func() { smtpWG.Wait(); close(serveDone) }()

	start("health checker", func() error { a.checker.Run(rootCtx); return nil })
	if a.runner != nil {
		start("dns discovery", func() error { a.runner.Run(rootCtx); return nil })
	}
	if a.adminLn != nil {
		start("admin api", func() error { return a.admin.Serve(rootCtx, a.adminLn) })
	}

	binds := make([]string, len(a.smtpLns))
	for i, nl := range a.smtpLns {
		binds[i] = nl.name + "=" + nl.ln.Addr().String()
	}
	lg.Info("bifrost listening", "version", version, "smtp", strings.Join(binds, ","),
		"admin", a.adminAddr(), "config", cfgPath)

	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigc)

	joined := make(chan struct{})
	go func() { wg.Wait(); close(joined) }()

	// stop is the process's last act, and it is BOUNDED. wg.Wait() is not:
	// proxy.Serve only returns once every session goroutine has, and a
	// session can legitimately still be running — a client dribbling
	// through the discard-to-dot path re-arms data_progress per chunk, so
	// it can outlast any drain deadline all the way to session_max. The
	// drain has already said goodbye by this point (backend legs aborted,
	// 421s written), so waiting past the goodbye grace buys nothing an
	// operator wants; exiting is what they asked for.
	stop := func(code int) int {
		cancelRoot()
		if !waitFor(joined, goodbyeGrace) {
			lg.Warn("components still running; exiting anyway", "grace", goodbyeGrace)
		}
		return code
	}
	for {
		select {
		case sig := <-sigc:
			if sig == syscall.SIGHUP {
				a.reload()
				continue
			}
			lg.Info("shutdown signal received", "signal", sig.String())
			a.drain(cancelSessions, serveDone)
			return stop(0)
		case err := <-errc:
			// A single listener dying outside a drain lands here (wrapped
			// with its own name, above) just as readily as any other
			// long-runner's error: errc is the primary detector now that
			// serveDone only closes once every listener has returned.
			lg.Error("component failed; shutting down", "error", err)
			return stop(1)
		case <-serveDone:
			// serveDone is "nothing in flight on any listener" — the
			// drain's own signal, consumed inside a.drain before this
			// select ever sees it. Reaching it here with no drain
			// underway means every listener returned cleanly on its own
			// (each already reported itself via errc above, but the
			// smtpWG-closer goroutine can win the race and close this
			// first): still nothing left to serve, still an error.
			lg.Error("smtp listener stopped unexpectedly")
			return stop(1)
		}
	}
}

// runCheck implements -c: load and validate the config at path, printing
// every diagnostic (errors and warnings alike) to stderr with its
// file:line:col, and "config OK" to stdout on success. It exits 0 when
// there are only warnings — R1 requires -c to fail loading only on
// errors, per PROJECT.md's warn/reject split.
func runCheck(path string, stdout, stderr io.Writer) int {
	if path == "" {
		_, _ = fmt.Fprintln(stderr, "bifrost: -c requires -f <path>")
		return 2
	}

	_, diags := config.Load(path)
	for _, d := range diags {
		_, _ = fmt.Fprintln(stderr, d.Error())
	}
	if diags.HasErrors() {
		return 1
	}

	_, _ = fmt.Fprintln(stdout, "config OK")
	return 0
}
