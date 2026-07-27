package httpbench

import (
	"errors"
	"os"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"time"
)

// RouteMetrics is the endpoint every implementation serves, whatever language
// it is written in. It answers a JSON object of the counters in [Sample]; a
// value an implementation cannot report is left out rather than zeroed, and the
// requester reports it as unavailable.
//
// The endpoint is an out-of-band measurement: it is hit on its own connection,
// before and after a measured window, never during one. Reading it costs
// something on every implementation, and what it costs differs between them, so
// a run samples twice back to back once and subtracts that from every pair.
const RouteMetrics = "/metrics"

// Sample is a server's memory at one instant. Values are cumulative where the
// runtime reports them that way, so a caller takes two samples and subtracts.
type Sample struct {
	Nanos        int64
	AllocBytes   uint64 // Cumulative bytes allocated, /gc/heap/allocs:bytes.
	AllocObjects uint64 // Cumulative objects allocated.
	LiveHeap     uint64 // Live heap after a GC, /memory/classes/heap/objects:bytes.
	Stacks       uint64 // Goroutine stacks in use.
	TotalMem     uint64 // Everything the runtime has mapped.
	GCCycles     uint64
	GCCPUSeconds float64
	Goroutines   uint64
	Threads      uint64 // OS threads created, which a blocking syscall server buys.
}

// sampleNames are the runtime metrics a Go implementation reports. They are
// read through the metrics package rather than [runtime.ReadMemStats] so that
// nothing stops the world, and so the numbers are exact counters instead of
// sampled estimates.
var sampleNames = [...]string{
	"/gc/heap/allocs:bytes",
	"/gc/heap/allocs:objects",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/stacks:bytes",
	"/memory/classes/total:bytes",
	"/gc/cycles/total:gc-cycles",
	"/cpu/classes/gc/total:cpu-seconds",
	"/sched/goroutines:goroutines",
}

// Sampler reads the metrics above. It holds its slices so that answering the
// endpoint does not allocate.
type Sampler struct {
	samples []metrics.Sample
}

// NewSampler prepares a sampler, failing if the runtime does not report one of
// the metrics the benchmark is built around.
func NewSampler() (*Sampler, error) {
	supported := make(map[string]bool, len(sampleNames))
	for _, d := range metrics.All() {
		supported[d.Name] = true
	}
	s := &Sampler{samples: make([]metrics.Sample, 0, len(sampleNames))}
	for _, name := range sampleNames {
		if !supported[name] {
			return nil, errors.New("runtime does not report " + name)
		}
		s.samples = append(s.samples, metrics.Sample{Name: name})
	}
	return s, nil
}

// Read takes a sample. A collection runs first so the live heap reported is
// what survived, not what has yet to be swept; that collection is why a sample
// is only ever taken outside a measured window, and why the two GC cycles a
// window costs are the same on both sides of a subtraction.
func (s *Sampler) Read(dst *Sample) {
	runtime.GC()
	metrics.Read(s.samples)
	*dst = Sample{
		Nanos:        time.Now().UnixNano(),
		AllocBytes:   s.samples[0].Value.Uint64(),
		AllocObjects: s.samples[1].Value.Uint64(),
		LiveHeap:     s.samples[2].Value.Uint64(),
		Stacks:       s.samples[3].Value.Uint64(),
		TotalMem:     s.samples[4].Value.Uint64(),
		GCCycles:     s.samples[5].Value.Uint64(),
		GCCPUSeconds: s.samples[6].Value.Float64(),
		Goroutines:   s.samples[7].Value.Uint64(),
		Threads:      uint64(pprof.Lookup("threadcreate").Count()),
	}
}

// AppendJSON appends a sample as a JSON object. It is formatted by hand, with
// one typed helper per kind of value: a helper taking `any` would box every
// counter it is handed, which is one allocation per key on the side of the
// comparison whose whole claim is that it does not allocate.
func (smp *Sample) AppendJSON(dst []byte) []byte {
	dst = append(dst, `{"nanos":`...)
	dst = strconv.AppendInt(dst, smp.Nanos, 10)
	dst = appendUintField(dst, "allocBytes", smp.AllocBytes)
	dst = appendUintField(dst, "allocObjects", smp.AllocObjects)
	dst = appendUintField(dst, "liveHeap", smp.LiveHeap)
	dst = appendUintField(dst, "stacks", smp.Stacks)
	dst = appendUintField(dst, "totalMem", smp.TotalMem)
	dst = appendUintField(dst, "gcCycles", smp.GCCycles)
	dst = appendFloatField(dst, "gcCPUSeconds", smp.GCCPUSeconds)
	dst = appendUintField(dst, "goroutines", smp.Goroutines)
	dst = appendUintField(dst, "threads", smp.Threads)
	return append(dst, '}')
}

func appendUintField(dst []byte, name string, v uint64) []byte {
	dst = appendKey(dst, name)
	return strconv.AppendUint(dst, v, 10)
}

func appendFloatField(dst []byte, name string, v float64) []byte {
	dst = appendKey(dst, name)
	return strconv.AppendFloat(dst, v, 'f', 6, 64)
}

func appendKey(dst []byte, name string) []byte {
	dst = append(dst, ',', '"')
	dst = append(dst, name...) // Keys are constants here, no escaping needed.
	return append(dst, '"', ':')
}

// ParseSample reads back a body written by [Sample.AppendJSON]. Values are
// found by name rather than by position, so an implementation may order or omit
// them, and nothing is allocated: the requester parses samples on the same
// machine it is measuring.
func ParseSample(dst *Sample, body []byte) error {
	*dst = Sample{}
	nanos, err := jsonNumberField(body, "nanos")
	if err != nil {
		return err
	}
	dst.Nanos = int64(nanos)
	uints := [...]struct {
		name string
		dst  *uint64
	}{
		{"allocBytes", &dst.AllocBytes},
		{"allocObjects", &dst.AllocObjects},
		{"liveHeap", &dst.LiveHeap},
		{"stacks", &dst.Stacks},
		{"totalMem", &dst.TotalMem},
		{"gcCycles", &dst.GCCycles},
		{"goroutines", &dst.Goroutines},
		{"threads", &dst.Threads},
	}
	for _, f := range uints {
		v, err := jsonNumberField(body, f.name)
		if err != nil {
			return err
		}
		*f.dst = uint64(v)
	}
	dst.GCCPUSeconds, err = jsonNumberField(body, "gcCPUSeconds")
	return err
}

var errNoField = errors.New("metrics answer missing field")

// jsonNumberField finds `"name":` in body and parses the number after it.
func jsonNumberField(body []byte, name string) (float64, error) {
	i := indexKey(body, name)
	if i < 0 {
		return 0, errors.Join(errNoField, errors.New(name))
	}
	rest := body[i:]
	end := 0
	for end < len(rest) && (rest[end] == '-' || rest[end] == '.' || (rest[end] >= '0' && rest[end] <= '9')) {
		end++
	}
	if end == 0 {
		return 0, errors.Join(errNoField, errors.New(name))
	}
	return strconv.ParseFloat(string(rest[:end]), 64)
}

// indexKey returns the offset just past `"name":` in body, or -1.
func indexKey(body []byte, name string) int {
	for i := 0; i+len(name)+3 <= len(body); i++ {
		if body[i] != '"' || string(body[i+1:i+1+len(name)]) != name {
			continue
		}
		rest := body[i+1+len(name):]
		if len(rest) >= 2 && rest[0] == '"' && rest[1] == ':' {
			return i + len(name) + 3
		}
	}
	return -1
}

// ProcStatus is what the kernel says a process is holding. It needs no
// cooperation from the server at all, which makes it the one measurement every
// implementation can be held to. It is coarse: it belongs next to the
// allocation numbers, not instead of them.
type ProcStatus struct {
	RSS  uint64 // VmRSS: resident set size now, bytes.
	Peak uint64 // VmHWM: the high water mark of the above, bytes.
}

// ReadProcStatus reads VmRSS and VmHWM out of /proc/<pid>/status.
func ReadProcStatus(pid int, dst *ProcStatus) error {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return err
	}
	*dst = ProcStatus{}
	dst.RSS, err = procStatusField(b, "VmRSS:")
	if err != nil {
		return err
	}
	dst.Peak, err = procStatusField(b, "VmHWM:")
	return err
}

// procStatusField reads one "Name:\t   1234 kB" line, in bytes.
func procStatusField(status []byte, name string) (uint64, error) {
	rest := status
	for len(rest) > 0 {
		line := rest
		if nl := indexByte(line, '\n'); nl >= 0 {
			line, rest = line[:nl], rest[nl+1:]
		} else {
			rest = nil
		}
		if len(line) < len(name) || string(line[:len(name)]) != name {
			continue
		}
		fields := line[len(name):]
		start := 0
		for start < len(fields) && (fields[start] == ' ' || fields[start] == '\t') {
			start++
		}
		end := start
		for end < len(fields) && fields[end] >= '0' && fields[end] <= '9' {
			end++
		}
		kB, err := strconv.ParseUint(string(fields[start:end]), 10, 64)
		if err != nil {
			return 0, err
		}
		return kB * 1024, nil // /proc reports these in kB.
	}
	return 0, errors.New("/proc status has no " + name)
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
