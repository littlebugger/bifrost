package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run implements the CLI over explicit stdout/stderr writers (rather
// than touching os.Stdout/os.Stderr or calling os.Exit itself), the same
// shape cmd/bifrost uses, so it can be driven in-process by a test.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "target SMTP address, host:port (required)")
	conns := fs.Int("c", 10, "concurrent long-lived connections")
	msgs := fs.Int("m", 10, "messages sent per connection")
	rate := fs.Float64("rate", 0, "messages/sec each connection paces itself to via its own time.Ticker; total demand scales with -c (0 = unpaced)")
	size := fs.Int("size", 512, "DATA body size in bytes")
	pipeline := fs.Bool("pipeline", false, "pipeline MAIL/RCPT/DATA per message instead of sending them one at a time")
	direct := fs.Bool("direct", false, "label this run as a direct-against-backend baseline; purely informational -- the wire protocol loadgen speaks is identical whether the far end is a fake backend or the proxy, by design (R4)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *addr == "" {
		_, _ = fmt.Fprintln(stderr, "loadgen: -addr is required")
		return 2
	}
	if *conns <= 0 || *msgs <= 0 || *size < 0 {
		_, _ = fmt.Fprintln(stderr, "loadgen: -c and -m must be > 0, -size must be >= 0")
		return 2
	}

	mode := "proxy"
	if *direct {
		mode = "direct"
	}
	_, _ = fmt.Fprintf(stderr, "loadgen: mode=%s addr=%s c=%d m=%d rate=%v size=%d pipeline=%v\n",
		mode, *addr, *conns, *msgs, *rate, *size, *pipeline)

	res := Run(Config{Addr: *addr, Conns: *conns, Msgs: *msgs, Rate: *rate, Size: *size, Pipeline: *pipeline})
	if err := json.NewEncoder(stdout).Encode(res); err != nil {
		_, _ = fmt.Fprintf(stderr, "loadgen: encode result: %v\n", err)
		return 1
	}
	return 0
}
