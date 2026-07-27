// Command httpbenchplot draws the figures of a run measured by
// [github.com/soypat/httpbench/cmd/httpbench].
//
// It is a separate command because it is a separate dependency: gonum's plotting
// stack has no business in the program that starts and measures servers, and
// none at all in a server binary whose size is one of the results.
//
//	go run ./cmd/httpbenchplot -json=results.json -dir=plots
//
// Five figures, in the order a reader should meet them:
//
//	heap-per-request.png   the headline: what an answer costs, per workload
//	header-flood.png       what a server spends against what it was sent
//	static-footprint.png   binary, resident set and memory per idle connection
//	throughput.png         requests per second against open connections
//	latency.png            p99 against open connections
//
// Allocations per request is deliberately not drawn: it tells the same story as
// the heap figure, and the count is in the table for anyone who wants it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/soypat/httpbench/internal/report"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "httpbenchplot:", err)
		os.Exit(1)
	}
}

func run() error {
	in := flag.String("json", "results.json", "run report to draw")
	dir := flag.String("dir", "plots", "directory the figures are written to")
	flag.Parse()
	rep, err := report.Read(*in)
	if err != nil {
		return err
	}
	return drawAll(rep, *dir)
}

// drawAll writes every figure of a report. A figure the report has no rows for
// is skipped rather than drawn empty.
func drawAll(rep *report.Report, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	d := newDrawer(rep)
	for _, fig := range []struct {
		name string
		draw func(*report.Report, string) error
	}{
		{"heap-per-request.png", d.drawPerRequest},
		{"header-flood.png", d.drawFlood},
		{"static-footprint.png", d.drawStaticFootprint},
		{"throughput.png", d.drawThroughput},
		{"latency.png", d.drawLatency},
	} {
		path := filepath.Join(dir, fig.name)
		err := fig.draw(rep, path)
		if errors.Is(err, errNoRows) {
			fmt.Fprintf(os.Stderr, "skipping %s: report has no rows for it\n", fig.name)
			continue
		} else if err != nil {
			return fmt.Errorf("%s: %w", fig.name, err)
		}
		fmt.Fprintln(os.Stderr, "wrote", path)
	}
	return nil
}

var errNoRows = errors.New("no rows for this figure")
