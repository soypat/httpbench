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
	"sort"
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
	// Pooled by pointer for the reason the httphi server gives: a slice put
	// into a [sync.Pool] boxes its header, and that allocation would be
	// counted against the handler rather than against the harness.
	server.bufferPool.New = func() any {
		buf := make([]byte, flags.UserBufferSize)
		return &buf
	}
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
	mux.HandleFunc("GET /query", sv.HandleQuery)
	mux.HandleFunc("POST /form", sv.HandleForm)
	mux.HandleFunc("POST /multipart", sv.HandleMultipart)
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
	pooled := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooled)
	buf := *pooled
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

var notPresent = []byte("not present")

// HandleQuery answers with one parameter of the query string, decoded, the way
// a Go server reads one: net/http parses the whole query into a map of slices
// of strings to hand back a single value.
func (sv *Server) HandleQuery(w http.ResponseWriter, r *http.Request) {
	body := notPresent
	if query := r.URL.Query(); query.Has("query") {
		body = []byte(query.Get("query"))
	}
	writeBody(w, body)
}

// HandleForm answers with every pair of an urlencoded body, decoded, one per
// line.
//
// The pairs come back sorted by key, because [http.Request.PostForm] is a map
// and a map has no order to preserve. The corpus sends its pairs in that same
// order so the answer can be compared with a server that kept wire order; the
// sort is what a handler over a map has to do to get there.
func (sv *Server) HandleForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	keys := make([]string, 0, len(r.PostForm))
	for key := range r.PostForm {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pooled := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooled)
	body := (*pooled)[:0]
	for _, key := range keys {
		for _, value := range r.PostForm[key] {
			body = append(body, key...)
			body = append(body, '=')
			body = append(body, value...)
			body = append(body, '\n')
		}
	}
	writeBody(w, body)
}

// HandleMultipart answers with one line per part: its name, its filename when
// it has one, and its content.
//
// It streams with [http.Request.MultipartReader] rather than ParseMultipartForm,
// which is the fair comparison: the latter buffers what fits and spills the
// rest to files in the temporary directory, so what it costs is not only memory.
func (sv *Server) HandleMultipart(w http.ResponseWriter, r *http.Request) {
	parts, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad multipart", http.StatusBadRequest)
		return
	}
	pooled := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooled)
	pooledOut := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooledOut)
	buf := *pooled
	body := (*pooledOut)[:0]
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			http.Error(w, "bad part", http.StatusBadRequest)
			return
		}
		body = append(body, part.FormName()...)
		if name := part.FileName(); name != "" {
			body = append(body, ';')
			body = append(body, name...)
		}
		body = append(body, '=')
		for {
			n, err := part.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break // Part ended, or the body did; the next NextPart tells which.
			}
		}
		body = append(body, '\n')
	}
	writeBody(w, body)
}

// writeBody answers 200 with body, framed by its length. Nothing is written
// until the whole answer is known, so a handler that fails halfway can still
// answer with a status instead of a truncated body.
func writeBody(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

// HandleMetrics answers the sample the orchestrator reads, in the same shape
// and from the same counters as every other Go implementation.
func (sv *Server) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	var smp httpbench.Sample
	sv.sampler.Read(&smp)
	pooled := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooled)
	body := smp.AppendJSON((*pooled)[:0])
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

func (sv *Server) AcquireUserBuffer() *[]byte    { return sv.bufferPool.Get().(*[]byte) }
func (sv *Server) ReleaseUserBuffer(buf *[]byte) { sv.bufferPool.Put(buf) }
