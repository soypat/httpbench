// Command httpbench measures what an HTTP server spends per request, per
// connection and at rest, with one server binary per implementation.
//
// A server is built, started, sampled, driven and killed by this program; it
// links none of it. That is what makes the size of a server binary and the
// memory of a server process results in their own right: a binary contains one
// stack and a process runs one stack, rather than every implementation being
// linked into the same measurement.
//
//	go run ./cmd/httpbench run                     # whole matrix, Markdown out
//	go run ./cmd/httpbench drive -addr=… -scenario=echo
//	go run ./cmd/httpbench sample -addr=…          # one memory sample
//	go run ./cmd/httpbench render -json=results.json
//
// Conformance is not measured yet: the request corpus and the digest every
// server answers with still live in lneto. Until they move here, a row says
// what a server spent, not that it parsed correctly.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/soypat/httpbench"
	"github.com/soypat/httpbench/internal/driver"
	"github.com/soypat/httpbench/internal/report"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "httpbench:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("want a subcommand: run, drive, sample or render")
	}
	switch args[0] {
	case "run":
		return runMatrix(args[1:])
	case "drive":
		return runDrive(args[1:])
	case "sample":
		return runSample(args[1:])
	case "render":
		return runRender(args[1:])
	}
	return errors.New("unknown subcommand " + args[0])
}

func runMatrix(args []string) error {
	cfg := defaultMatrix()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	impls := fs.String("impls", strings.Join(cfg.impls, ","), "implementations to measure")
	transports := fs.String("transports", strings.Join(cfg.transports, ","), "net, raw or both")
	echoSizes := fs.String("echo", intList(cfg.echoSizes), "echo body sizes, bytes")
	floodSizes := fs.String("flood", intList(cfg.floodSizes), "header block sizes for the flood probe, bytes")
	conns := fs.String("conns", intList(cfg.concurrency), "connection counts for the scaling rows")
	fs.IntVar(&cfg.requests, "requests", cfg.requests, "requests per row")
	fs.IntVar(&cfg.warmup, "warmup", cfg.warmup, "warmup requests before each measured row")
	fs.IntVar(&cfg.workloadConns, "workload-conns", cfg.workloadConns, "connections the per-scenario rows use")
	fs.IntVar(&cfg.holdConns, "hold-conns", cfg.holdConns, "connections held open for the idle measurement")
	fs.IntVar(&cfg.szReq, "sz-req", cfg.szReq, "-sz-req given to every server")
	fs.IntVar(&cfg.szUsr, "sz-usr", cfg.szUsr, "-sz-usr given to every server")
	fs.IntVar(&cfg.workers, "J", cfg.workers, "-J given to every server")
	fs.StringVar(&cfg.binDir, "bin", cfg.binDir, "where server binaries are built")
	fs.StringVar(&cfg.jsonOut, "json", "results.json", "where the measurements are kept")
	fs.StringVar(&cfg.out, "o", "", "where the Markdown report is written, empty for stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.impls = splitList(*impls)
	cfg.transports = splitList(*transports)
	var err error
	if cfg.echoSizes, err = parseInts(*echoSizes); err != nil {
		return err
	}
	if cfg.floodSizes, err = parseInts(*floodSizes); err != nil {
		return err
	}
	if cfg.concurrency, err = parseInts(*conns); err != nil {
		return err
	}

	var rows []report.Result
	mapping := make(map[string]string, len(cfg.impls))
	for _, impl := range cfg.impls {
		// Always rebuilt, so a stale binary is never measured and the size
		// recorded is the size of what was just run.
		path, size, err := buildImpl(cfg, impl)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "built %s: %s\n", impl, humanFloatBytes(float64(size)))
		for _, transport := range cfg.transports {
			got, name, flagMap, err := measureServer(cfg, path, size, impl, transport)
			if err != nil {
				return fmt.Errorf("%s over %s: %w", impl, transport, err)
			}
			rows = append(rows, got...)
			// Keyed by the name the server announced, which is what the rows
			// and the figures call it.
			mapping[name] = flagMap
		}
	}

	rep := report.New(buildFlags, report.Limits{
		RequestBufferSize: cfg.szReq,
		UserBufferSize:    cfg.szUsr,
		FixedGoroutines:   cfg.workers,
	}, cfg.requests, rows, mapping)
	// The measurements outlive this process: a report can be redrawn or
	// compared against a later run without measuring anything again.
	if cfg.jsonOut != "" {
		if err = report.Write(cfg.jsonOut, rep); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "wrote", cfg.jsonOut)
	}
	markdown := renderReport(rep)
	if cfg.out == "" {
		fmt.Print(markdown)
		return nil
	}
	return os.WriteFile(cfg.out, []byte(markdown), 0o644)
}

// runDrive sends one scenario at a server someone else started, which is how a
// server is poked at by hand without a matrix around it.
func runDrive(args []string) error {
	fs := flag.NewFlagSet("drive", flag.ExitOnError)
	addr := fs.String("addr", "", "address of a server to drive")
	scenario := fs.String("scenario", driver.ScenarioHello,
		"hello, echo, query, form, multipart or headerflood")
	size := fs.Int("size", 1024, "echo body size or flood header block size, bytes")
	conns := fs.Int("conns", 1, "connections held open in parallel")
	requests := fs.Int("requests", 2000, "requests in total across connections")
	keepAlive := fs.Bool("keepalive", false, "send every request of a connection down the same socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr == "" {
		return errors.New("drive needs -addr")
	}
	var set *driver.RequestSet
	switch *scenario {
	case driver.ScenarioHello:
		set = driver.HelloSet(8)
	case driver.ScenarioEcho:
		set = driver.EchoSet(8, *size)
	case driver.ScenarioQuery:
		set = driver.QuerySet(8)
	case driver.ScenarioForm:
		set = driver.FormSet(8)
	case driver.ScenarioMultipart:
		set = driver.MultipartSet(8)
	case driver.ScenarioFlood:
		set = driver.FloodSet(4, *size)
	default:
		return errors.New("unknown scenario " + *scenario)
	}
	res := driver.RunLoad(driver.Config{
		Addr: *addr, Set: set, Conns: *conns, Requests: *requests, KeepAlive: *keepAlive,
		ReadBuffer: readBufferFor(set),
	})
	if res.Err != nil {
		return res.Err
	}
	fmt.Printf("%s: %d requests, %.0f req/s, p50 %s, p99 %s, %d drops, statuses %s\n",
		set.Scenario, res.Requests, res.Throughput(),
		res.Percentile(0.5).Round(time.Microsecond),
		res.Percentile(0.99).Round(time.Microsecond),
		res.Drops, formatStatuses(res.Statuses))
	return nil
}

// runSample reads one out-of-band sample, and reports what reading it cost by
// taking a second one straight after.
func runSample(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ExitOnError)
	addr := fs.String("addr", "", "address of a server to sample")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr == "" {
		return errors.New("sample needs -addr")
	}
	var first, second httpbench.Sample
	if err := driver.Sample(*addr, &first, 30*time.Second); err != nil {
		return err
	}
	if err := driver.Sample(*addr, &second, 30*time.Second); err != nil {
		return err
	}
	fmt.Printf("live heap %s, total mapped %s, %d goroutines, %d threads, %d GC cycles\n",
		humanFloatBytes(float64(second.LiveHeap)), humanFloatBytes(float64(second.TotalMem)),
		second.Goroutines, second.Threads, second.GCCycles)
	fmt.Printf("one sample of this server allocates %s in it\n",
		humanFloatBytes(float64(second.AllocBytes-first.AllocBytes)))
	return nil
}

// runRender redraws the Markdown from measurements taken earlier.
func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	in := fs.String("json", "results.json", "measurements to render")
	out := fs.String("o", "", "where the Markdown is written, empty for stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rep, err := report.Read(*in)
	if err != nil {
		return err
	}
	markdown := renderReport(rep)
	if *out == "" {
		fmt.Print(markdown)
		return nil
	}
	return os.WriteFile(*out, []byte(markdown), 0o644)
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, part := range splitList(s) {
		v, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, errors.New("empty list: " + s)
	}
	return out, nil
}

func intList(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}
