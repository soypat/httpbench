package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/soypat/httpbench/internal/driver"
	"github.com/soypat/httpbench/internal/report"
)

// renderReport writes the Markdown a reader meets first. Every table says what
// it was measured under, because a number from this benchmark without its
// limits beside it is not a measurement of anything.
func renderReport(rep *report.Report) string {
	var b strings.Builder
	b.WriteString("# httpbench\n\n")
	fmt.Fprintf(&b, "%s, %s/%s, GOMAXPROCS=%d, %s requests per row.\n\n",
		rep.GoVersion, rep.GOOS, rep.GOARCH, rep.GOMAXPROCS, humanCount(float64(rep.Requests)))
	fmt.Fprintf(&b, "Servers built with `%s`, started with `-sz-req=%d -sz-usr=%d -J=%d`.\n\n",
		rep.BuildFlags, rep.Limits.RequestBufferSize, rep.Limits.UserBufferSize, rep.Limits.FixedGoroutines)

	// What those flags were made to mean, per implementation. The two stacks
	// are not configurable in the same terms and the report says so rather than
	// implying a trade that was never made.
	b.WriteString("Flag mapping:\n\n")
	for _, impl := range sortedKeys(rep.FlagMapping) {
		fmt.Fprintf(&b, "- `%s`: %s\n", impl, rep.FlagMapping[impl])
	}
	b.WriteString("\n")

	writeStaticTable(&b, rep.Results)
	writeRequestTable(&b, rep.Results)
	writeConcurrencyTable(&b, rep.Results)
	writeIdleTable(&b, rep.Results)
	writeFloodTable(&b, rep.Results)
	return b.String()
}

// writeStaticTable is what one binary per implementation buys: a size, a
// resident set and a heap that belong to one stack instead of to a process with
// every stack linked into it.
func writeStaticTable(b *strings.Builder, rows []report.Result) {
	b.WriteString("## Static footprint\n\n")
	b.WriteString("| impl | transport | binary | RSS at rest | RSS peak | live heap | goroutines | threads | keep-alive | metrics call |\n")
	b.WriteString("|---|---|--:|--:|--:|--:|--:|--:|:-:|--:|\n")
	for _, r := range rows {
		if r.Kind != report.KindStatic {
			continue
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %.0f | %d | %s | %s |\n",
			r.Impl, r.Transport, humanFloatBytes(float64(r.BinaryBytes)),
			humanFloatBytes(float64(r.RSSAtRest)), humanFloatBytes(float64(r.RSSPeak)),
			humanFloatBytes(r.RestLiveHeap), r.RestGoroutines, r.Threads,
			yesNo(r.KeepAlive), humanFloatBytes(r.EndpointHeapCost))
	}
	b.WriteString("\nThe metrics call column is what one out-of-band sample allocates on that " +
		"server; every per-request figure below has two of them subtracted.\n\n")
}

func writeRequestTable(b *strings.Builder, rows []report.Result) {
	b.WriteString("## Per request\n\n")
	b.WriteString("| impl | transport | scenario | mode | conns | answered | refused | wire | heap/req | allocs/req | GC/kreq | GC CPU | p50 | p99 | req/s | statuses |\n")
	b.WriteString("|---|---|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|\n")
	for _, r := range rows {
		if r.Kind != "" || strings.Contains(r.Scenario, "@") || strings.HasPrefix(r.Scenario, "flood/") {
			continue
		}
		writeWorkloadRow(b, r)
	}
	b.WriteString("\n")
}

func writeConcurrencyTable(b *strings.Builder, rows []report.Result) {
	b.WriteString("## Against open connections\n\n")
	b.WriteString("| impl | transport | scenario | mode | conns | answered | refused | wire | heap/req | allocs/req | GC/kreq | GC CPU | p50 | p99 | req/s | statuses |\n")
	b.WriteString("|---|---|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|\n")
	for _, r := range rows {
		if r.Kind != "" || !strings.Contains(r.Scenario, "@") {
			continue
		}
		writeWorkloadRow(b, r)
	}
	b.WriteString("\n")
}

func writeFloodTable(b *strings.Builder, rows []report.Result) {
	b.WriteString("## Header flood\n\n")
	b.WriteString("A probe, not a conformance run: a bounded server refuses these and an " +
		"unbounded one serves them, and what reaching either answer costs is the measurement. " +
		"Where every attempt was refused before an answer came back, the per-request figures " +
		"are per attempt.\n\n")
	b.WriteString("| impl | transport | scenario | mode | conns | answered | refused | wire | heap/req | allocs/req | GC/kreq | GC CPU | p50 | p99 | req/s | statuses |\n")
	b.WriteString("|---|---|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|\n")
	for _, r := range rows {
		if r.Kind != "" || !strings.HasPrefix(r.Scenario, "flood/") {
			continue
		}
		writeWorkloadRow(b, r)
	}
	b.WriteString("\n")
}

func writeWorkloadRow(b *strings.Builder, r report.Result) {
	fmt.Fprintf(b, "| %s | %s | %s | %s | %d | %d | %d | %s | %s | %.1f | %.2f | %.1f%% | %s | %s | %s | %s |\n",
		r.Impl, r.Transport, r.Scenario, r.Mode, r.Conns, r.Requests, r.Drops,
		humanBytes(r.WireBytes), humanFloatBytes(r.HeapPerReq), r.AllocsPerReq,
		r.GCCyclesPerKReq, 100*r.GCCPUFraction,
		shortDuration(time.Duration(r.P50Nanos)), shortDuration(time.Duration(r.P99Nanos)),
		humanCount(r.Throughput), formatStatuses(r.Statuses))
}

func writeIdleTable(b *strings.Builder, rows []report.Result) {
	any := false
	for _, r := range rows {
		if r.Kind == report.KindIdle {
			any = true
			break
		}
	}
	if !any {
		return
	}
	b.WriteString("## Per open, idle connection\n\n")
	b.WriteString("| impl | transport | conns held | heap/conn | stack/conn | goroutines/conn | threads |\n")
	b.WriteString("|---|---|--:|--:|--:|--:|--:|\n")
	for _, r := range rows {
		if r.Kind != report.KindIdle {
			continue
		}
		fmt.Fprintf(b, "| %s | %s | %d | %s | %s | %.2f | %d |\n",
			r.Impl, r.Transport, r.Conns, humanFloatBytes(r.HeapPerConn),
			humanFloatBytes(r.StackPerConn), r.GoroPerConn, r.Threads)
	}
	b.WriteString("\n")
}

func formatStatuses(sc []driver.StatusCount) string {
	if len(sc) == 0 {
		return "-"
	}
	sorted := append([]driver.StatusCount(nil), sc...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Code < sorted[j].Code })
	var b strings.Builder
	for i, s := range sorted {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d×%s", s.Code, humanCount(float64(s.Count)))
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func humanBytes(n int) string { return humanFloatBytes(float64(n)) }

func humanFloatBytes(v float64) string {
	switch {
	case v < 0:
		return "-" + humanFloatBytes(-v)
	case v < 1024:
		return trimFloat(v) + " B"
	case v < 1024*1024:
		return trimFloat(v/1024) + " kB"
	}
	return trimFloat(v/(1024*1024)) + " MB"
}

func humanCount(v float64) string {
	switch {
	case v < 1000:
		return trimFloat(v)
	case v < 1e6:
		return trimFloat(v/1000) + "k"
	}
	return trimFloat(v/1e6) + "M"
}

func trimFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func shortDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Microsecond:
		return strconv.FormatInt(int64(d), 10) + "ns"
	case d < time.Millisecond:
		return trimFloat(float64(d)/float64(time.Microsecond)) + "µs"
	case d < time.Second:
		return trimFloat(float64(d)/float64(time.Millisecond)) + "ms"
	}
	return trimFloat(d.Seconds()) + "s"
}
