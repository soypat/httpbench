package driver

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/soypat/httpbench"
)

// Config describes one measured run against a server.
type Config struct {
	Addr string
	Set  *RequestSet
	// Conns is how many connections drive the load in parallel.
	Conns int
	// Requests is the total across all connections.
	Requests int
	// KeepAlive sends every request of a connection down the same socket. A
	// server serving one exchange per connection is driven with it off, since a
	// connection per request is what it actually does.
	KeepAlive bool
	// Hold keeps every connection open and idle after its requests are done,
	// until Release is closed. It is how the cost of an open connection is
	// weighed: the server is sampled while they are all still there.
	Hold    bool
	Release chan struct{}
	// Ready is closed once every connection has finished its requests. Only
	// meaningful together with Hold.
	Ready chan struct{}
	// ReadBuffer is how much answer a connection can hold at once. Zero takes a
	// default large enough for the scenarios here.
	ReadBuffer int
}

// Result is what a run measured on the requester's side.
type Result struct {
	Requests  int
	Elapsed   time.Duration
	Latencies []time.Duration // Sorted.
	Statuses  []StatusCount
	BytesOut  int64
	BytesIn   int64
	// Drops counts connections the server hung up on instead of answering.
	Drops int
	Err   error
}

// StatusCount is how many answers of a code a run saw.
type StatusCount struct {
	Code  int `json:"code"`
	Count int `json:"count"`
}

func (r *Result) Throughput() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Requests) / r.Elapsed.Seconds()
}

func (r *Result) Percentile(p float64) time.Duration {
	if len(r.Latencies) == 0 {
		return 0
	}
	return r.Latencies[int(p*float64(len(r.Latencies)-1))]
}

func (r *Result) addStatus(code, count int) {
	for i := range r.Statuses {
		if r.Statuses[i].Code == code {
			r.Statuses[i].Count += count
			return
		}
	}
	r.Statuses = append(r.Statuses, StatusCount{Code: code, Count: count})
}

// How a requester treats a refused connection. A server serving from a fixed
// pool of exchanges refuses what it has no room for, so a refusal is a retry,
// not an error, until they stop coming altogether.
const (
	maxRefusals = 400
	// spinRefusals are retried straight away: a worker freeing up takes
	// microseconds, and sleeping through that would measure the sleep.
	spinRefusals = 8
	refusalWait  = 50 * time.Microsecond
)

const defaultReadBuffer = 512 << 10

// RunLoad drives a server and checks every answer that comes back. A mismatch
// stops the run: a number measured against a server that answered wrongly is
// not worth reporting.
func RunLoad(cfg Config) *Result {
	perConn := cfg.Requests / cfg.Conns
	if perConn < 1 {
		perConn = 1
	}
	results := make([]*Result, cfg.Conns)
	var wg, served sync.WaitGroup
	served.Add(cfg.Conns)
	start := time.Now()
	for i := 0; i < cfg.Conns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = runConn(cfg, perConn, i, &served)
		}(i)
	}
	if cfg.Ready != nil {
		// Tell the caller when every connection has served its requests. In
		// hold mode they are all still open at that point, which is the state
		// worth sampling the server in.
		go func() {
			served.Wait()
			close(cfg.Ready)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := &Result{Elapsed: elapsed}
	for _, r := range results {
		if r == nil {
			continue
		}
		if r.Err != nil && total.Err == nil {
			total.Err = r.Err
		}
		total.Requests += r.Requests
		total.BytesIn += r.BytesIn
		total.BytesOut += r.BytesOut
		total.Drops += r.Drops
		total.Latencies = append(total.Latencies, r.Latencies...)
		for _, sc := range r.Statuses {
			total.addStatus(sc.Code, sc.Count)
		}
	}
	sort.Slice(total.Latencies, func(i, j int) bool { return total.Latencies[i] < total.Latencies[j] })
	return total
}

// dial opens a connection to the server under test. Lingering is turned off so
// a run that opens a connection per request closes them with a reset instead of
// leaving tens of thousands of sockets in TIME_WAIT, which would run the
// requester out of ephemeral ports long before the run is over.
func dial(addr string) (net.Conn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetLinger(0)
	}
	return conn, nil
}

// runConn serves one connection's share of the load. Everything it needs is
// allocated before the first request goes out.
func runConn(cfg Config, requests, index int, served *sync.WaitGroup) *Result {
	res := &Result{Latencies: make([]time.Duration, 0, requests)}
	servedOnce := sync.OnceFunc(served.Done)
	defer servedOnce()
	var conn net.Conn
	var err error
	if cfg.KeepAlive {
		conn, err = dial(cfg.Addr)
		if err != nil {
			res.Err = err
			return res
		}
	}
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	readBuffer := cfg.ReadBuffer
	if readBuffer <= 0 {
		readBuffer = defaultReadBuffer
	}
	buf := make([]byte, 0, readBuffer)
	var resp Response
	for i := 0; i < requests; i++ {
		variant := (index + i) % len(cfg.Set.wire)
		wire := cfg.Set.wire[variant]
		sent := time.Now()
		for attempt := 0; ; attempt++ {
			if conn == nil {
				// A connection per request, or a fresh one after a refusal.
				if conn, err = dial(cfg.Addr); err != nil {
					res.Err = err
					return res
				}
			}
			if _, err = conn.Write(wire); err == nil {
				buf, err = readResponse(conn, buf[:0], &resp)
			}
			if err == nil {
				break
			}
			// A server with every worker busy refuses the connection instead
			// of queueing it: that is its backpressure, not a failure. Give it
			// back and try again, counting the refusal.
			conn.Close()
			conn = nil
			res.Drops++
			if cfg.Set.IsProbe() {
				break // Probes measure the parse, not the answer.
			}
			if attempt >= maxRefusals {
				res.Err = fmt.Errorf("%s: refused %d times in a row: %w",
					cfg.Set.Scenario, attempt+1, err)
				return res
			}
			if attempt >= spinRefusals {
				time.Sleep(refusalWait)
			}
		}
		if conn == nil {
			continue // Refused probe: nothing was answered to record.
		}
		res.BytesOut += int64(len(wire))
		res.Latencies = append(res.Latencies, time.Since(sent))
		res.BytesIn += int64(len(buf))
		res.Requests++
		res.addStatus(resp.Status, 1)
		if err = cfg.Set.Check(variant, &resp); err != nil {
			res.Err = fmt.Errorf("%s variant %d: %w", cfg.Set.Scenario, variant, err)
			return res
		}
		if !cfg.KeepAlive {
			// The connection served its one request. Keeping it would send the
			// next request down a socket the server has already closed, which
			// reads back as a refusal that never happened.
			conn.Close()
			conn = nil
		}
	}
	if cfg.Hold {
		// The connection stays open and idle: what it costs the server to keep
		// is the number this mode exists to weigh.
		servedOnce()
		<-cfg.Release
	}
	return res
}

// readResponse reads until one whole answer is in buf. A server that answers a
// refusal by closing the connection is reported as such rather than as a
// timeout.
func readResponse(conn net.Conn, buf []byte, resp *Response) ([]byte, error) {
	for {
		n, err := conn.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if n > 0 {
			if _, perr := ParseResponse(resp, buf); perr == nil {
				return buf, nil
			} else if !errors.Is(perr, errShortResponse) {
				return buf, perr
			}
		}
		if err != nil {
			return buf, err
		}
		if len(buf) == cap(buf) {
			return buf, errors.New("answer larger than the requester's buffer")
		}
	}
}

// SupportsKeepAlive reports whether a server answers a second request on the
// same connection. It is asked rather than assumed: a server serving one
// exchange per connection is not broken, it is a different design, and the
// difference belongs in the report instead of in a list of names here.
func SupportsKeepAlive(addr string) (bool, error) {
	conn, err := dial(addr)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return false, err
	}
	set := HelloSet(1)
	buf := make([]byte, 0, 8<<10)
	var resp Response
	for i := 0; i < 2; i++ {
		if _, err = conn.Write(set.wire[0]); err != nil {
			return i > 0, nil // Wrote one, could not write the second: no keep-alive.
		}
		buf, err = readResponse(conn, buf[:0], &resp)
		if err != nil {
			return false, nil
		}
		if err = set.Check(0, &resp); err != nil {
			return false, fmt.Errorf("server answered its own route wrongly: %w", err)
		}
	}
	return true, nil
}

// Sample reads one out-of-band memory sample from a server, on a connection of
// its own. It is never called inside a measured window: reading it costs the
// server a garbage collection and a syscall, which is why what it costs is
// itself measured and subtracted.
func Sample(addr string, dst *httpbench.Sample, timeout time.Duration) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if _, err = conn.Write(MetricsRequest); err != nil {
		return err
	}
	var resp Response
	buf, err := readResponse(conn, make([]byte, 0, 8<<10), &resp)
	if err != nil {
		return fmt.Errorf("reading sample: %w", err)
	}
	if resp.Status != 200 {
		return fmt.Errorf("metrics endpoint answered %d", resp.Status)
	}
	if err = httpbench.ParseSample(dst, resp.Body); err != nil {
		return fmt.Errorf("%w: %s", err, buf)
	}
	return nil
}
