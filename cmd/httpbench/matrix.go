package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/soypat/httpbench"
	"github.com/soypat/httpbench/internal/driver"
	"github.com/soypat/httpbench/internal/report"
)

// endpointCost is what one out-of-band sample costs the server it is read
// from, in bytes and in objects. It differs between implementations, so every
// measured window has two of them subtracted rather than being quoted with the
// weight of its own measurement still in it.
type endpointCost struct {
	bytes   float64
	objects float64
}

// buildFlags are what every implementation is built with. Stripping and
// trimming both sides is what makes the size column a comparison rather than a
// note about who had more debug information linked in.
const buildFlags = "-trimpath -ldflags=-s -w"

// matrixConfig is what a full run measures. Defaults are sized so a run takes a
// couple of minutes and still averages over enough requests to be stable.
type matrixConfig struct {
	impls      []string
	transports []string
	// echoSizes are the body sizes the echo scenario is measured at.
	echoSizes []int
	// floodSizes are header block sizes for the flood probe.
	floodSizes []int
	// concurrency is how many connections the scaling rows use.
	concurrency []int
	requests    int
	warmup      int
	// workloadConns is how many connections the per-scenario rows use. It
	// matches the server's worker count by default so neither implementation is
	// measured while the requester waits on the other end of its own
	// backpressure.
	workloadConns int
	holdConns     int

	// Standard server flags, passed to every implementation unchanged.
	szReq   int
	szUsr   int
	workers int

	binDir  string
	jsonOut string
	out     string
}

func defaultMatrix() matrixConfig {
	return matrixConfig{
		impls:         []string{"httphi", "nethttp"},
		transports:    []string{report.TransportNet, report.TransportRaw},
		echoSizes:     []int{64, 1 << 10},
		floodSizes:    []int{4 << 10, 64 << 10, 1 << 20},
		concurrency:   []int{1, 8, 64, 256},
		requests:      20000,
		warmup:        2000,
		workloadConns: 4,
		holdConns:     256,
		szReq:         4 << 10,
		szUsr:         8 << 10,
		workers:       4,
		binDir:        "bin",
	}
}

// childServer is a server running in its own process, so the memory sampled is
// the server's and nothing else: the requester's buffers, its goroutines and
// its own garbage live over here.
type childServer struct {
	cmd  *exec.Cmd
	addr string
	pid  int
	// impl is the name the server announced itself under, which is what the
	// report calls it. It need not be the directory it was built from: an
	// implementation names the stack it serves with, not its own path.
	impl    string
	trans   string
	flagMap string
	stderr  strings.Builder
}

// buildImpl builds one implementation and returns its path and size. It is
// rebuilt for every matrix so a stale binary is never measured, and the size is
// recorded at that moment.
func buildImpl(cfg matrixConfig, impl string) (path string, size int64, err error) {
	if err = os.MkdirAll(cfg.binDir, 0o755); err != nil {
		return "", 0, err
	}
	path = cfg.binDir + "/" + impl
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", path, "./implementations/"+impl)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", 0, fmt.Errorf("building %s: %w\n%s", impl, err, out)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	return path, info.Size(), nil
}

// startServer launches a built implementation and waits for it to report the
// address it is listening on. Port zero lets the kernel choose, so a run never
// waits on a port the last one has not let go of yet.
func startServer(cfg matrixConfig, path, impl, transport string) (*childServer, error) {
	cmd := exec.Command(path,
		"-addr=127.0.0.1:0",
		"-raw="+strconv.FormatBool(transport == report.TransportRaw),
		"-sz-req="+strconv.Itoa(cfg.szReq),
		"-sz-usr="+strconv.Itoa(cfg.szUsr),
		"-J="+strconv.Itoa(cfg.workers),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &childServer{cmd: cmd, impl: impl, trans: transport}
	cmd.Stderr = &c.stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	ready := make(chan string, 1)
	go readReadyLine(stdout, ready)
	select {
	case line := <-ready:
		if line == "" {
			c.stop()
			return nil, errors.New("server exited before listening: " + c.stderr.String())
		}
		var announced string
		c.addr, c.pid, announced, c.flagMap = parseReadyLine(line)
		if announced != "" {
			c.impl = announced
		}
	case <-time.After(10 * time.Second):
		c.stop()
		return nil, errors.New("server did not report an address: " + c.stderr.String())
	}
	if c.addr == "" {
		c.stop()
		return nil, errors.New("server reported no address")
	}
	return c, nil
}

// readReadyLine waits for the one line a server writes on stdout, and drains
// the rest so a server that does write more is never blocked by a full pipe.
func readReadyLine(r io.Reader, ready chan<- string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	sent := false
	for sc.Scan() {
		if !sent && strings.HasPrefix(sc.Text(), httpbench.ReadyPrefix) {
			ready <- sc.Text()
			sent = true
		}
	}
	if !sent {
		close(ready)
	}
}

// parseReadyLine picks the address, the process to sample, the name the server
// goes by and the flag mapping out of the line it announces itself with.
func parseReadyLine(line string) (addr string, pid int, impl, flagMap string) {
	for _, f := range strings.Fields(strings.TrimPrefix(line, httpbench.ReadyPrefix)) {
		switch {
		case strings.HasPrefix(f, "addr="):
			addr = strings.TrimPrefix(f, "addr=")
		case strings.HasPrefix(f, "pid="):
			pid, _ = strconv.Atoi(strings.TrimPrefix(f, "pid="))
		case strings.HasPrefix(f, "impl="):
			impl = strings.TrimPrefix(f, "impl=")
		case strings.HasPrefix(f, "flags="):
			flagMap = strings.TrimPrefix(f, "flags=")
		}
	}
	return addr, pid, impl, flagMap
}

func (c *childServer) stop() {
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}
}

// sample reads one out-of-band sample from the server, on a connection of its
// own and never inside a measured window.
func (c *childServer) sample(dst *httpbench.Sample) error {
	return driver.Sample(c.addr, dst, 30*time.Second)
}

// proc reads what the kernel says the server is holding, which needs no
// cooperation from the server at all.
func (c *childServer) proc(dst *httpbench.ProcStatus) error {
	return httpbench.ReadProcStatus(c.pid, dst)
}

// measure runs one workload row: warm the server, sample, load, sample.
// Everything between the samples is the server serving the requests being
// counted, and nothing else touches it in that time.
func measure(c *childServer, set *driver.RequestSet, cfg matrixConfig, conns, requests int, keepAlive bool, cost endpointCost) (report.Result, error) {
	load := driver.Config{
		Addr: c.addr, Set: set, Conns: conns, Requests: cfg.warmup, KeepAlive: keepAlive,
		ReadBuffer: readBufferFor(set),
	}
	if warm := driver.RunLoad(load); warm.Err != nil {
		return report.Result{}, fmt.Errorf("warmup: %w", warm.Err)
	}
	var before, after httpbench.Sample
	if err := c.sample(&before); err != nil {
		return report.Result{}, err
	}
	load.Requests = requests
	res := driver.RunLoad(load)
	if res.Err != nil {
		return report.Result{}, res.Err
	}
	if err := c.sample(&after); err != nil {
		return report.Result{}, err
	}
	// A probe that was refused outright still measured something: what refusing
	// costs. Charge the window to the attempts in that case, since that is what
	// the server actually did with its memory.
	served := float64(res.Requests)
	if served == 0 {
		if !set.IsProbe() || res.Drops == 0 {
			return report.Result{}, errors.New("no request was answered")
		}
		served = float64(res.Drops)
	}
	elapsed := float64(after.Nanos-before.Nanos) / 1e9
	// The pair of samples costs what one sample costs, twice over: subtract it
	// so the row is the server serving, not the server being asked about it.
	heap := float64(after.AllocBytes-before.AllocBytes) - 2*cost.bytes
	objects := float64(after.AllocObjects-before.AllocObjects) - 2*cost.objects
	row := report.Result{
		Impl: c.impl, Transport: c.trans, Scenario: set.Scenario,
		Mode: report.Mode(keepAlive), Conns: conns,
		Requests: res.Requests, Drops: res.Drops, WireBytes: set.WireBytes(),
		HeapPerReq:      heap / served,
		AllocsPerReq:    objects / served,
		GCCyclesPerKReq: 1000 * float64(after.GCCycles-before.GCCycles) / served,
		P50Nanos:        res.Percentile(0.5).Nanoseconds(),
		P99Nanos:        res.Percentile(0.99).Nanoseconds(),
		Throughput:      res.Throughput(),
		Statuses:        res.Statuses,
		Threads:         int(after.Threads),
	}
	if elapsed > 0 {
		row.GCCPUFraction = (after.GCCPUSeconds - before.GCCPUSeconds) / elapsed
	}
	return row, nil
}

// measureIdleConns weighs what an open, idle keep-alive connection costs the
// server: the memory it holds and the goroutine it occupies while nothing at
// all is happening on it.
func measureIdleConns(c *childServer, set *driver.RequestSet, conns int) (report.Result, error) {
	var before, after httpbench.Sample
	if err := c.sample(&before); err != nil {
		return report.Result{}, err
	}
	cfg := driver.Config{
		Addr:       c.addr,
		Set:        set,
		Conns:      conns,
		Requests:   conns, // One request each, then hold the connection open.
		KeepAlive:  true,  // The point is the connection outliving its request.
		Hold:       true,
		Release:    make(chan struct{}),
		Ready:      make(chan struct{}),
		ReadBuffer: readBufferFor(set),
	}
	done := make(chan *driver.Result, 1)
	go func() { done <- driver.RunLoad(cfg) }()
	select {
	case <-cfg.Ready:
	case <-time.After(60 * time.Second):
		close(cfg.Release)
		<-done
		return report.Result{}, errors.New("connections never finished their first request")
	}
	time.Sleep(200 * time.Millisecond) // Let the server settle into idle.
	err := c.sample(&after)
	close(cfg.Release)
	res := <-done
	if err != nil {
		return report.Result{}, err
	}
	if res.Err != nil {
		return report.Result{}, res.Err
	}
	n := float64(conns)
	return report.Result{
		Impl: c.impl, Transport: c.trans, Scenario: "idle conns", Kind: report.KindIdle,
		Mode: report.Mode(true), Conns: conns, Requests: res.Requests,
		HeapPerConn:  float64(int64(after.LiveHeap)-int64(before.LiveHeap)) / n,
		StackPerConn: float64(int64(after.Stacks)-int64(before.Stacks)) / n,
		GoroPerConn:  float64(int64(after.Goroutines)-int64(before.Goroutines)) / n,
		Threads:      int(after.Threads),
		Statuses:     res.Statuses,
	}, nil
}

// measureServer measures one implementation over one transport, from a process
// started for it and killed after it. No row inherits the pools another one
// warmed.
func measureServer(cfg matrixConfig, path string, size int64, dir, transport string) ([]report.Result, string, string, error) {
	c, err := startServer(cfg, path, dir, transport)
	if err != nil {
		return nil, "", "", err
	}
	defer c.stop()
	// From here on the implementation is called what it calls itself.
	impl := c.impl

	// What the endpoint itself costs on this implementation, weighed by
	// sampling back to back with nothing in between but the next request.
	//
	// The first call is thrown away: it pays for whatever the server allocates
	// once, the pools and buffers a first exchange fills, and charging that to
	// every window would flatter the servers that have most of it. Of the pairs
	// that follow the smallest is taken, because a pair that happened to
	// contain a collection is measuring the collection, not the endpoint.
	var smp httpbench.Sample
	if err = c.sample(&smp); err != nil {
		return nil, "", "", err
	}
	cost := endpointCost{bytes: math.MaxFloat64, objects: math.MaxFloat64}
	for i, prev := 0, smp; i < 5; i++ {
		if err = c.sample(&smp); err != nil {
			return nil, "", "", err
		}
		if d := float64(smp.AllocBytes - prev.AllocBytes); d < cost.bytes {
			cost.bytes = d
		}
		if d := float64(smp.AllocObjects - prev.AllocObjects); d < cost.objects {
			cost.objects = d
		}
		prev = smp
	}

	var proc httpbench.ProcStatus
	if err = c.proc(&proc); err != nil {
		return nil, "", "", err
	}
	static := report.Result{
		Impl: impl, Transport: transport, Scenario: "at rest", Kind: report.KindStatic,
		BinaryBytes: size, RSSAtRest: proc.RSS,
		RestLiveHeap: float64(smp.LiveHeap), RestTotalMem: float64(smp.TotalMem),
		RestGoroutines: float64(smp.Goroutines), Threads: int(smp.Threads),
		EndpointHeapCost: cost.bytes,
	}

	// A connection per request is the mode every implementation supports, so it
	// is the one they are compared in. Keep-alive is measured as well wherever
	// it is served, since that is how net/http is normally deployed — and
	// whether it is served is asked of the server rather than assumed from its
	// name, so an implementation added later needs no special case here.
	modes := []bool{false}
	keepAlive, err := driver.SupportsKeepAlive(c.addr)
	if err != nil {
		return nil, "", "", fmt.Errorf("probing keep-alive: %w", err)
	}
	if keepAlive {
		modes = append(modes, true)
	}
	static.KeepAlive = keepAlive
	var rows []report.Result
	sets := []*driver.RequestSet{driver.HelloSet(8)}
	for _, size := range cfg.echoSizes {
		sets = append(sets, driver.EchoSet(8, size))
	}
	// The parsing workloads, in order of what they make the server take apart:
	// a query string, an urlencoded body, then a body whose parts declare no
	// length at all.
	sets = append(sets, driver.QuerySet(8), driver.FormSet(8), driver.MultipartSet(8))
	for _, set := range sets {
		for _, keepAlive := range modes {
			row, err := measure(c, set, cfg, cfg.workloadConns, cfg.requests, keepAlive, cost)
			if err != nil {
				return nil, "", "", fmt.Errorf("scenario %s: %w", set.Scenario, err)
			}
			rows = append(rows, row)
			fmt.Fprintf(os.Stderr, "  %-8s %-4s %-12s %-10s %9s/req %6.1f allocs/req %9.0f req/s\n",
				impl, transport, set.Scenario, row.Mode,
				humanFloatBytes(row.HeapPerReq), row.AllocsPerReq, row.Throughput)
		}
	}

	hello := driver.HelloSet(8)
	for _, conns := range cfg.concurrency {
		for _, keepAlive := range modes {
			row, err := measure(c, hello, cfg, conns, cfg.requests, keepAlive, cost)
			if err != nil {
				return nil, "", "", fmt.Errorf("concurrency %d: %w", conns, err)
			}
			row.Scenario = "hello@" + strconv.Itoa(conns)
			rows = append(rows, row)
		}
	}

	idle, err := measureIdleConns(c, hello, cfg.holdConns)
	if err != nil {
		return nil, "", "", fmt.Errorf("idle connections: %w", err)
	}
	rows = append(rows, idle)
	fmt.Fprintf(os.Stderr, "  %-8s %-4s idle conns: %s heap, %s stack, %.2f goroutines each\n",
		impl, transport, humanFloatBytes(idle.HeapPerConn), humanFloatBytes(idle.StackPerConn), idle.GoroPerConn)

	for _, blockSize := range cfg.floodSizes {
		set := driver.FloodSet(4, blockSize)
		requests := cfg.requests
		if blockSize >= 1<<20 {
			requests = 2000 // A megabyte a request is enough of them.
		}
		row, err := measure(c, set, cfg, cfg.workloadConns, requests, false, cost)
		if err != nil {
			return nil, "", "", fmt.Errorf("flood %d: %w", blockSize, err)
		}
		row.Scenario = "flood/" + humanBytes(blockSize)
		rows = append(rows, row)
		fmt.Fprintf(os.Stderr, "  %-8s %-4s %-12s %9s/req %8.0f allocs/req %s\n",
			impl, transport, row.Scenario, humanFloatBytes(row.HeapPerReq),
			row.AllocsPerReq, formatStatuses(row.Statuses))
	}

	// The high water mark is read last: it is the largest the process ever got
	// while serving everything above.
	if err = c.proc(&proc); err != nil {
		return nil, "", "", err
	}
	static.RSSPeak = proc.Peak
	return append([]report.Result{static}, rows...), impl, c.flagMap, nil
}

// readBufferFor sizes the requester's per-connection buffer to the answer it
// expects, with room for a flood probe's refusal.
func readBufferFor(set *driver.RequestSet) int {
	size := set.WireBytes() + 64<<10
	if size < 128<<10 {
		size = 128 << 10
	}
	return size
}
