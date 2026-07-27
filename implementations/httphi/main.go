package main

import (
	"fmt"
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
	mux.Reset(4) // Routes registered below, plus room for one more.

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
	server.bufferPool.New = func() any { return make([]byte, flags.UserBufferSize) }
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
	sampler    *httpbench.Sampler
}

func (sv *Server) RegisterHandlers(mux *httphi.MuxSlice, flags httpbench.Flags) {
	mux.Handle("GET /hello-world", sv.HandleHelloWorld)
	mux.Handle("/echo", sv.HandleEcho)
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
	buf := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(buf)
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

func (sv *Server) HandleQuery(ex *httphi.Exchange) {
	buf := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(buf)
	query, present := ex.AppendQuery(buf[:0], "query", true)
	if present {
		ex.WriteBody(query)
	} else {
		ex.WriteBody(notPresent)
	}
}

func (sv *Server) HandleForm(ex *httphi.Exchange) {
	var form httpraw.Form
	buf := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(buf)
	wbuf := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(wbuf)
	ex.RequestParseForm(&form, buf)
	form.Decode()
	for i := range form.Len() {
		k, v := form.Pair(i)
		wbuf = append(wbuf, k...)
		wbuf = append(wbuf, '=')
		wbuf = append(wbuf, v...)
		wbuf = append(wbuf, '\n')
	}
	ex.WriteBody(wbuf)
}

// HandleMetrics answers the sample every implementation owes the orchestrator.
// It formats into a pooled buffer with [httpbench.Sample.AppendJSON], which
// allocates nothing, so what the endpoint costs is a syscall and a GC rather
// than a number that grows with how often it is read.
func (sv *Server) HandleMetrics(ex *httphi.Exchange) {
	var smp httpbench.Sample
	sv.sampler.Read(&smp)
	buf := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(buf)
	body := smp.AppendJSON(buf[:0])
	ex.StageHeader("Content-Type", "application/json")
	ex.StageHeaderInt("Content-Length", int64(len(body)), 10)
	ex.WriteBody(body)
}

func (sv *Server) AcquireUserBuffer() []byte    { return sv.bufferPool.Get().([]byte) }
func (sv *Server) ReleaseUserBuffer(buf []byte) { sv.bufferPool.Put(buf) }

func backoff(consecutiveBackoffs uint) time.Duration {
	return min(time.Millisecond, 1<<consecutiveBackoffs)
}
