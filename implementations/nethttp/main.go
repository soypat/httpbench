// Command nethttp serves the benchmark routes with net/http, over the same two
// transports the other implementations are measured on. It is the reference
// every other implementation is read against: what it costs is what a Go server
// costs when it is written the way Go servers are written.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/soypat/httpbench"
)

// implName is the name this server is reported under.
const implName = "stdlib"

// flagMapping states which standard flags this stack has a counterpart for.
// Most of them are httφ's memory, taken at configuration time, and net/http has
// nothing to map them onto: it allocates per request instead. Saying so is the
// point, so the report prints this line rather than implying a fair trade.
const flagMapping = "sz-req=MaxHeaderBytes,sz-usr=handler-buffer,raw=rawsock-or-net,J=unmapped(goroutine-per-conn)"

func main() {
	var flags httpbench.Flags
	err := flags.RegisterAndParseCommandline()
	if err != nil {
		log.Fatal("parsing flags:", err)
	}
	err = run(flags)
	if err != nil {
		log.Fatal(err)
	}
}

func run(flags httpbench.Flags) error {
	var server Server
	server.bufferPool.New = func() any { return make([]byte, flags.UserBufferSize) }
	var err error
	server.sampler, err = httpbench.NewSampler()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	server.RegisterHandlers(mux, flags)

	listener, err := httpbench.Listen(flags)
	if err != nil {
		return err
	}
	fmt.Println(flags.ReadyLine(listener.Addr(), implName, flagMapping))
	os.Stdout.Sync()
	srv := &http.Server{
		Handler: mux,
		// The nearest counterpart -sz-req has here: a limit on what a request
		// header may be, rather than the memory one is parsed into.
		MaxHeaderBytes: flags.RequestBufferSize,
		// A chatty server would drown the ready line the orchestrator reads,
		// so failed exchanges are silent unless someone is debugging one.
		ErrorLog: log.New(debugWriter(), "", 0),
	}
	return srv.Serve(listener)
}

type Server struct {
	bufferPool sync.Pool
	sampler    *httpbench.Sampler
}

func (sv *Server) RegisterHandlers(mux *http.ServeMux, flags httpbench.Flags) {
	mux.HandleFunc("GET /hello-world", sv.HandleHelloWorld)
	mux.HandleFunc("/echo", sv.HandleEcho)
	mux.HandleFunc("GET "+httpbench.RouteMetrics, sv.HandleMetrics)
}

var helloWorld = []byte("hello world")

func (sv *Server) HandleHelloWorld(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", strconv.Itoa(len(helloWorld)))
	w.Write(helloWorld)
}

// HandleEcho streams the request body back a buffer at a time, the same shape
// and the same buffer size the other implementations echo with, so the memory
// the answer costs is a property of the stack rather than of the handler.
func (sv *Server) HandleEcho(w http.ResponseWriter, r *http.Request) {
	// Writing an answer while the request body is still being read is what
	// echoing is: without this, net/http closes the body once the response
	// reaches the wire, as [http.ResponseWriter] warns it may, and the answer
	// is silently short by whatever had not been read yet.
	http.NewResponseController(w).EnableFullDuplex()
	buf := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(buf)
	if r.ContentLength <= 0 {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
	for read := int64(0); read < r.ContentLength; {
		toRead := buf
		if remain := r.ContentLength - read; remain < int64(len(buf)) {
			toRead = buf[:remain]
		}
		n, err := r.Body.Read(toRead)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			read += int64(n)
		}
		if err != nil {
			return // Short body: the answer is already framed by Content-Length.
		}
	}
}

// HandleMetrics answers the sample the orchestrator reads, in the same shape
// and from the same counters as every other Go implementation.
func (sv *Server) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	var smp httpbench.Sample
	sv.sampler.Read(&smp)
	buf := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(buf)
	body := smp.AppendJSON(buf[:0])
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

// debugWriter is where net/http's own complaints go: nowhere, unless
// HTTPBENCH_DEBUG is set, in which case stderr.
func debugWriter() io.Writer {
	if os.Getenv("HTTPBENCH_DEBUG") != "" {
		return os.Stderr
	}
	return io.Discard
}

func (sv *Server) AcquireUserBuffer() []byte    { return sv.bufferPool.Get().([]byte) }
func (sv *Server) ReleaseUserBuffer(buf []byte) { sv.bufferPool.Put(buf) }
