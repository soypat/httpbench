// Package report is what a run measured, in a form that survives a file: the
// orchestrator writes it, the table renderer and the plotter read it, and a run
// need not happen in the same process as whatever is drawn from it.
package report

import (
	"encoding/json"
	"os"
	"runtime"
	"time"

	"github.com/soypat/httpbench/internal/driver"
)

// Result is a measured row in a form that survives a file, so a run and
// whatever is drawn from it need not happen in the same process.
type Result struct {
	Impl      string `json:"impl"`
	Transport string `json:"transport"`
	Scenario  string `json:"scenario"`
	Mode      string `json:"mode,omitempty"`
	Conns     int    `json:"conns,omitempty"`
	Requests  int    `json:"requests,omitempty"`
	Drops     int    `json:"drops,omitempty"`
	WireBytes int    `json:"wireBytes,omitempty"`

	HeapPerReq      float64              `json:"heapPerReq,omitempty"`
	AllocsPerReq    float64              `json:"allocsPerReq,omitempty"`
	GCCyclesPerKReq float64              `json:"gcCyclesPerKReq,omitempty"`
	GCCPUFraction   float64              `json:"gcCPUFraction,omitempty"`
	P50Nanos        int64                `json:"p50Nanos,omitempty"`
	P99Nanos        int64                `json:"p99Nanos,omitempty"`
	Throughput      float64              `json:"throughput,omitempty"`
	Statuses        []driver.StatusCount `json:"statuses,omitempty"`

	// Kind marks the rows that are not a workload: what the server costs
	// before it has served anything, and what an open connection costs it.
	Kind string `json:"kind,omitempty"`

	// Static rows carry what the implementation costs at rest, which is what
	// one binary per implementation makes measurable in the first place.
	BinaryBytes    int64   `json:"binaryBytes,omitempty"`
	RSSAtRest      uint64  `json:"rssAtRest,omitempty"`
	RSSPeak        uint64  `json:"rssPeak,omitempty"`
	RestLiveHeap   float64 `json:"restLiveHeap,omitempty"`
	RestTotalMem   float64 `json:"restTotalMem,omitempty"`
	RestGoroutines float64 `json:"restGoroutines,omitempty"`
	// KeepAlive is whether the server answered a second request on the same
	// connection, which is asked of it rather than assumed.
	KeepAlive bool `json:"keepAlive,omitempty"`
	// EndpointHeapCost is what one call to the metrics endpoint allocates on
	// this implementation, measured by sampling back to back. Both the heap and
	// the object counts below have two of these subtracted, because they are
	// the measurement's own weight and it differs between implementations.
	EndpointHeapCost float64 `json:"endpointHeapCost,omitempty"`

	// Idle rows carry what one open, idle keep-alive connection costs.
	HeapPerConn  float64 `json:"heapPerConn,omitempty"`
	StackPerConn float64 `json:"stackPerConn,omitempty"`
	GoroPerConn  float64 `json:"goroPerConn,omitempty"`

	Threads int `json:"threads,omitempty"`
}

// Row kinds. A row without one is a workload.
const (
	KindStatic = "static"
	KindIdle   = "idle"
)

// Transports a server is measured over. "net" is the net package with its
// poller; "raw" is blocking syscalls straight to the socket, which is the shape
// TinyGo's net package has on a device.
const (
	TransportNet = "net"
	TransportRaw = "raw"
)

// How connections were used for a row.
const (
	ModeConnPerRequest = "conn/req"
	ModeKeepAlive      = "keep-alive"
)

// Mode names how connections were used.
func Mode(keepAlive bool) string {
	if keepAlive {
		return ModeKeepAlive
	}
	return ModeConnPerRequest
}

// ConnTypeName names a transport by the connection type the server was handed,
// which is what the memory a transport costs actually belongs to.
func ConnTypeName(transport string) string {
	if transport == TransportRaw {
		return "raw.Conn"
	}
	return "net.Conn"
}

// SeriesName names a measured line by the server, the connection type it was
// handed and how connections were used: "httphi+raw.Conn/req" is httφ, over a
// raw socket, one connection per request.
func SeriesName(impl, transport, mode string) string {
	name := impl + "+" + ConnTypeName(transport)
	switch mode {
	case ModeConnPerRequest:
		name += "/req"
	case ModeKeepAlive:
		name += " keep-alive"
	}
	return name
}

// Report is a whole run: what was measured, on what, and under which limits.
type Report struct {
	Generated  string `json:"generated"`
	GoVersion  string `json:"goVersion"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	GOMAXPROCS int    `json:"gomaxprocs"`
	// BuildFlags are the flags every implementation was built with. Both sides
	// are stripped and trimmed or the size column means nothing.
	BuildFlags string `json:"buildFlags"`
	// Limits are the standard flags every server was started with.
	Limits Limits `json:"limits"`
	// FlagMapping is each implementation's own statement of what those flags
	// were made to mean in it, printed because the two stacks are not
	// configurable in the same terms.
	FlagMapping map[string]string `json:"flagMapping"`
	Requests    int               `json:"requestsPerRow"`
	Results     []Result          `json:"results"`
}

// Limits are the knobs every server was given, so a report read later says
// what the numbers in it were measured under.
type Limits struct {
	RequestBufferSize int `json:"szReq"`
	UserBufferSize    int `json:"szUsr"`
	FixedGoroutines   int `json:"j"`
}

// New gathers the rows of a run together with the conditions they were measured
// under, which is the half of a report that stops being obvious a week later.
func New(buildFlags string, limits Limits, requestsPerRow int, rows []Result, mapping map[string]string) *Report {
	return &Report{
		Generated:   time.Now().Format(time.RFC3339),
		GoVersion:   runtime.Version(),
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		GOMAXPROCS:  runtime.GOMAXPROCS(0),
		BuildFlags:  buildFlags,
		Limits:      limits,
		FlagMapping: mapping,
		Requests:    requestsPerRow,
		Results:     rows,
	}
}

// Write saves a report where a later run, a renderer or a plotter can find it.
func Write(path string, rep *Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Read loads a report written by [Write].
func Read(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rep := new(Report)
	if err = json.Unmarshal(b, rep); err != nil {
		return nil, err
	}
	return rep, nil
}
