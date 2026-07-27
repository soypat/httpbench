package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/soypat/httpbench"
	"github.com/soypat/lneto/http/httphi"
	"github.com/soypat/lneto/http/httpraw"
)

// implName is the name this server is reported under.
const implName = "httphi"

// flagMapping is what the standard flags were made to mean here, printed in the
// report so a reader knows what the two stacks were given.
const flagMapping = "J=workers,sz-req=RequestHeaderBufferSize+ResponseHeaderMinBufferSize+KVCap(sz-req/32),sz-usr=handler-buffer,raw=rawsock-or-net"

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
	var mux httphi.MuxSlice
	mux.Reset(8) // Routes registered below, plus room for one more.

	var router httphi.Router
	err := router.Configure(httphi.RouterConfig{
		Mux:                         &mux,
		FixedNumGoroutines:          flags.FixedGoroutines,
		RequestHeaderBufferSize:     flags.RequestBufferSize,
		ResponseHeaderMinBufferSize: flags.RequestBufferSize,
		RequestNumHeaderKVCap:       flags.RequestBufferSize / 32,
		NormalizeOutgoingKeys:       false,
		MaxAwaitingConns:            flags.FixedGoroutines,
		Backoff:                     backoff,
	})
	if err != nil {
		return err
	}
	var server Server
	// Pooled by pointer: putting a slice into a [sync.Pool] boxes its header,
	// which is an allocation per request charged to the handler for something
	// the benchmark's own scaffolding did.
	server.bufferPool.New = func() any {
		buf := make([]byte, flags.UserBufferSize)
		return &buf
	}
	server.sampler, err = httpbench.NewSampler()
	if err != nil {
		return err
	}
	server.RegisterHandlers(&mux, flags)

	listener, err := httpbench.Listen(flags)
	if err != nil {
		return err
	}
	fmt.Println(flags.ReadyLine(listener.Addr(), implName, flagMapping))
	os.Stdout.Sync()
	defer router.TeardownGoroutines()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		err = router.Handle(conn)
		if err != nil {
			// Every worker is busy and the queue is full. Dropping the
			// connection is the backpressure that keeps memory bounded.
			conn.Close()
		}
	}
}

type Server struct {
	bufferPool sync.Pool
	// formPool and partPool keep the parsers' own storage between requests:
	// their key-value slices and the part headers' name buffers are what a
	// parser would otherwise allocate per request, and this stack's claim is
	// that it need not.
	formPool sync.Pool
	partPool sync.Pool
	sampler  *httpbench.Sampler
}

func (sv *Server) RegisterHandlers(mux *httphi.MuxSlice, flags httpbench.Flags) {
	mux.Handle("GET /hello-world", sv.HandleHelloWorld)
	mux.Handle("/echo", sv.HandleEcho)
	mux.Handle("GET /query", sv.HandleQuery)
	mux.Handle("POST /form", sv.HandleForm)
	mux.Handle("POST /multipart", sv.HandleMultipart)
	mux.Handle("GET "+httpbench.RouteMetrics, sv.HandleMetrics)
}

var helloWorld = []byte("hello world")

func (sv *Server) HandleHelloWorld(ex *httphi.Exchange) {
	// Every answer carries its length. Without it the answer is delimited by
	// the connection closing, which costs a round trip the measurement would
	// then be attributing to the server.
	ex.StageHeaderInt("Content-Length", int64(len(helloWorld)), 10)
	ex.WriteBody(helloWorld)
}

// HandleEcho streams the request body back a buffer at a time, so a body larger
// than the buffer costs the same memory as a small one.
//
// The first read comes before the response header is staged, and that order is
// not a preference: a staged field is written into the same raw buffer that
// still holds the part of the body which arrived alongside the request header,
// so staging while that surplus is unread hands the peer its own header back.
// One read drains the surplus because it can be no larger than the request
// buffer, which [httpbench.Flags.Validate] keeps no larger than this one.
func (sv *Server) HandleEcho(ex *httphi.Exchange) {
	pooled := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooled)
	buf := *pooled
	contentLength, _ := ex.RequestContentLength() // Absent means no body, not an error.
	if contentLength <= 0 {
		ex.StageHeaderInt("Content-Length", 0, 10)
		ex.WriteHeader(200)
		return
	}
	n, err := ex.ReadBody(buf)
	if err != nil && n == 0 {
		return
	}
	ex.StageHeaderInt("Content-Length", contentLength, 10)
	for read := 0; ; {
		if _, err = ex.WriteBody(buf[:n]); err != nil {
			return
		}
		read += n
		if int64(read) >= contentLength {
			return
		}
		toRead := buf
		if remain := contentLength - int64(read); remain < int64(len(buf)) {
			toRead = buf[:remain]
		}
		if n, err = ex.ReadBody(toRead); err != nil || n == 0 {
			return
		}
	}
}

var notPresent = []byte("not present")

// HandleQuery answers with one parameter of the query string, decoded. The
// value is appended into a pooled buffer that is already long enough for it, so
// what the answer costs is the parse and nothing else.
func (sv *Server) HandleQuery(ex *httphi.Exchange) {
	pooled := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooled)
	body, present := ex.AppendQuery((*pooled)[:0], "query", true)
	if !present {
		body = notPresent
	}
	ex.StageHeaderInt("Content-Length", int64(len(body)), 10)
	ex.WriteBody(body)
}

// HandleForm answers with every pair of an urlencoded body, decoded, one per
// line and in the order they arrived.
//
// The body is parsed in place, in a buffer the handler already had: a form is
// decoded by rewriting it, which only ever shrinks it, so the pairs cost no
// memory beyond the bytes that were read. The whole body is consumed before the
// answer's first field is staged, for the reason [Server.HandleEcho] gives.
func (sv *Server) HandleForm(ex *httphi.Exchange) {
	pooled := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooled)
	pooledOut := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooledOut)
	form := sv.AcquireForm()
	defer sv.ReleaseForm(form)
	err := ex.RequestParseForm(form, *pooled)
	if err == nil {
		err = form.Decode()
	}
	if err != nil {
		// A body that is not a form, or is longer than the buffer it would be
		// parsed in. Either way nothing was answered from it.
		writeStatus(ex, 400)
		return
	}
	body := (*pooledOut)[:0]
	for i := range form.Len() {
		k, v := form.Pair(i)
		body = append(body, k...)
		body = append(body, '=')
		body = append(body, v...)
		body = append(body, '\n')
	}
	ex.StageHeaderInt("Content-Length", int64(len(body)), 10)
	ex.WriteBody(body)
}

// HandleMultipart answers with one line per part of a "multipart/form-data"
// body: its name, its filename when it has one, and its content.
//
// A part declares no length, so the parts stream through the same buffer one at
// a time and a body of any size costs what one buffer costs. Only the answer
// grows with the request here, and it is written into a second pooled buffer.
func (sv *Server) HandleMultipart(ex *httphi.Exchange) {
	pooled := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooled)
	pooledOut := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooledOut)
	sink := sv.AcquirePartSink()
	defer sv.ReleasePartSink(sink)
	sink.body = (*pooledOut)[:0]
	parts, err := ex.ReadMultiparts(sink.parts[:0], *pooled, sink.begin)
	// Kept even on failure: the part headers hold buffers worth reusing, and
	// the next request finds them already sized.
	sink.parts = parts
	if err != nil {
		writeStatus(ex, 400)
		return
	}
	ex.StageHeaderInt("Content-Length", int64(len(sink.body)), 10)
	ex.WriteBody(sink.body)
}

// partSink is where every part of one multipart body is written: the answer
// itself, a line at a time. One sink serves all the parts of a request, since
// [httphi.Exchange.ReadMultiparts] reads them one after another.
type partSink struct {
	body  []byte
	parts []httphi.MultipartSink
}

// begin writes the line's key as the part opens, before any of its content has
// been read, which is the only point at which the header is still to hand.
func (s *partSink) begin(hdr *httpraw.MultipartHeader) io.WriteCloser {
	s.body = append(s.body, hdr.Name...)
	if len(hdr.Filename) > 0 {
		s.body = append(s.body, ';')
		s.body = append(s.body, hdr.Filename...)
	}
	s.body = append(s.body, '=')
	return s
}

func (s *partSink) Write(b []byte) (int, error) {
	s.body = append(s.body, b...)
	return len(b), nil
}

// Close ends the part's line. It is called when the part ends, so a line in the
// answer is a part that arrived whole.
func (s *partSink) Close() error {
	s.body = append(s.body, '\n')
	return nil
}

// writeStatus answers with a status and no body. The length is stated even when
// it is zero, so a requester never has to wait for the connection to close to
// know the answer ended.
func writeStatus(ex *httphi.Exchange, code int) {
	ex.StageHeaderInt("Content-Length", 0, 10)
	ex.WriteHeader(code)
}

// HandleMetrics answers the sample every implementation owes the orchestrator.
// It formats into a pooled buffer with [httpbench.Sample.AppendJSON], which
// allocates nothing, so what the endpoint costs is a syscall and a GC rather
// than a number that grows with how often it is read.
func (sv *Server) HandleMetrics(ex *httphi.Exchange) {
	var smp httpbench.Sample
	sv.sampler.Read(&smp)
	pooled := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(pooled)
	body := smp.AppendJSON((*pooled)[:0])
	ex.StageHeader("Content-Type", "application/json")
	ex.StageHeaderInt("Content-Length", int64(len(body)), 10)
	ex.WriteBody(body)
}

func (sv *Server) AcquireUserBuffer() *[]byte    { return sv.bufferPool.Get().(*[]byte) }
func (sv *Server) ReleaseUserBuffer(buf *[]byte) { sv.bufferPool.Put(buf) }

func (sv *Server) AcquireForm() *httpraw.Form {
	form, _ := sv.formPool.Get().(*httpraw.Form)
	if form == nil {
		form = new(httpraw.Form)
	}
	return form
}

func (sv *Server) ReleaseForm(form *httpraw.Form) { sv.formPool.Put(form) }

func (sv *Server) AcquirePartSink() *partSink {
	sink, _ := sv.partPool.Get().(*partSink)
	if sink == nil {
		sink = new(partSink)
	}
	return sink
}

func (sv *Server) ReleasePartSink(sink *partSink) { sv.partPool.Put(sink) }

func backoff(consecutiveBackoffs uint) time.Duration {
	return min(time.Millisecond, 1<<consecutiveBackoffs)
}
