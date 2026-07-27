package main

import (
	"fmt"
	"log"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/soypat/httpbench"
	"github.com/soypat/lneto/http/httphi"
)

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
	mux.Reset(4) // Number of

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
	server.RegisterHandlers(&mux, flags)

	listener, err := httpbench.Listen(flags)
	if err != nil {
		return err
	}
	fmt.Println(flags.ServerInfo())
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		err = router.Handle(conn)
		if err != nil {
			log.Println(err)
		}
	}
	return nil
}

type Server struct {
	bufferPool sync.Pool
}

func (sv *Server) RegisterHandlers(mux *httphi.MuxSlice, flags httpbench.Flags) {
	mux.Handle("GET /hello-world", sv.HandleHelloWorld)
	mux.Handle("/echo", sv.HandleEcho)
	mux.Handle("/memstat", sv.HandleMemstats)
}

func (sv *Server) HandleHelloWorld(ex *httphi.Exchange) {
	ex.WriteBody([]byte("hello world"))
}

func (sv *Server) HandleEcho(ex *httphi.Exchange) {
	buf := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(buf)
	for {
		n, err := ex.ReadBody(buf)
		if err != nil || n == 0 {
			return
		}
		n2, err := ex.WriteBody(buf[:n])
		if err != nil || n2 != n {
			return
		}
	}
}

// expose memory stat
func (sv *Server) HandleMemstats(ex *httphi.Exchange) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	buf := sv.AcquireUserBuffer()
	defer sv.ReleaseUserBuffer(buf)
	dst := append(buf[:0], '{')
	// Match field names so to easily use json package leveraging reflect.
	// buf = appendJSONDictKey(buf, "alloc", stats.Alloc, true)
	buf = appendJSONDictKey(dst, "TotalAlloc", stats.TotalAlloc, true)
	dst = appendJSONDictKey(dst, "Mallocs", stats.Mallocs, true)
	dst = appendJSONDictKey(dst, "Frees", stats.Frees, true)
	dst = append(dst, '}')
	ex.WriteBody(dst)
}

func appendJSONDictKey(dst []byte, key string, val any, termComma bool) []byte {
	dst = append(dst, '"')
	dst = append(dst, key...) // Simple, don't allow quotes or non-escapable characters.
	dst = append(dst, '"', ':')
	switch v := val.(type) {
	case int64:
		dst = strconv.AppendInt(dst, v, 10)
	case uint64:
		dst = strconv.AppendUint(dst, v, 10)

	}
	if termComma {
		dst = append(dst, ',')
	}
	return dst
}

func (sv *Server) AcquireUserBuffer() []byte    { return sv.bufferPool.Get().([]byte) }
func (sv *Server) ReleaseUserBuffer(buf []byte) { sv.bufferPool.Put(buf) }

func backoff(consecutiveBackoffs uint) time.Duration {
	return min(time.Millisecond, 1<<consecutiveBackoffs)
}
