package httpbench

import (
	"errors"
	"flag"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/soypat/lneto/x/rawsock"
)

// Standardizes http benchmark configuration.
type Flags struct {
	Port              uint16
	FixedGoroutines   int
	RequestBufferSize int
	UserBufferSize    int

	UseRawConn bool
}

func (flags *Flags) RegisterAndParseCommandline() error {
	var addr string
	flag.StringVar(&addr, "addr", ":8080", "Host address with port")
	flag.IntVar(&flags.FixedGoroutines, "J", runtime.GOMAXPROCS(0), "Fixed number of goroutines for processing if available")
	flag.IntVar(&flags.RequestBufferSize, "sz-req", 512, "Request header buffer size")
	flag.IntVar(&flags.UserBufferSize, "sz-usr", 1024, "User buffer size")
	flag.BoolVar(&flags.UseRawConn, "raw", true, "Use raw operating system call interface instead of net package.")
	flag.Parse()

	// Address parsing.
	ip, port, ok := strings.Cut(addr, ":")
	if !ok {
		return errors.New("no colon in -addr=ip:port")
	} else if ip == "" {
		addr = "127.0.0.1:" + port // localhost
	}
	addrp, err := netip.ParseAddrPort(addr)
	if err != nil {
		return err
	}
	// Port zero is not a mistake here: it lets the kernel pick, which is how
	// the orchestrator runs a server per row without handing out ports itself.
	flags.Port = addrp.Port()
	return flags.Validate()
}

// Validate rejects flag combinations no implementation is expected to serve
// correctly, so a misconfiguration is a startup error rather than a row of
// numbers measured against a server answering the wrong bytes.
func (flags *Flags) Validate() error {
	if flags.RequestBufferSize <= 0 || flags.UserBufferSize <= 0 || flags.FixedGoroutines <= 0 {
		return errors.New("-sz-req, -sz-usr and -J must all be positive")
	}
	if flags.UserBufferSize < flags.RequestBufferSize {
		// A handler streaming a body reads what arrived alongside the request
		// header out of the request buffer in one go. Keeping the user buffer
		// at least that large is what makes one read enough.
		return errors.New("-sz-usr must be at least -sz-req")
	}
	return nil
}

// ReadyPrefix marks the one line a server writes on stdout once it is
// listening. A server writes nothing else there: the orchestrator reads this
// line to learn where to send requests and which process to sample.
const ReadyPrefix = "httpbench listening "

// ReadyLine formats that line. flagMap is the implementation's own statement of
// which of these flags it honours and which have no counterpart in it: the two
// stacks are not configurable in the same terms, so what the mapping was gets
// printed in the report rather than assumed.
func (flags *Flags) ReadyLine(addr net.Addr, impl, flagMap string) string {
	return ReadyPrefix + "addr=" + addr.String() +
		" impl=" + impl +
		" pid=" + strconv.Itoa(os.Getpid()) +
		" flags=" + flagMap
}

type TCPListener struct {
	rawconn     bool
	rawconnpool sync.Pool
	rawlistener rawsock.Listener

	stdlistener net.Listener
}

func Listen(flags Flags) (l *TCPListener, err error) {
	l = &TCPListener{
		rawconn: flags.UseRawConn,
	}
	if l.rawconn {
		err = l.rawlistener.Listen(flags.Port)
		l.rawconnpool.New = l.newRawConn
	} else {
		l.stdlistener, err = net.Listen("tcp", ":"+strconv.Itoa(int(flags.Port)))
	}
	if err != nil {
		return nil, err // A failed bind is not a working listener.
	}
	return l, nil
}

func (l *TCPListener) Accept() (conn net.Conn, err error) {
	if l.rawconn {
		rconn := l.rawconnpool.Get().(*rawConn)
		err = l.rawlistener.AcceptConn(&rconn.Conn)
		if err != nil {
			l.rawconnpool.Put(rconn) // Nothing was accepted into it.
			return nil, err
		}
		rconn.closed = false
		conn = rconn
	} else {
		conn, err = l.stdlistener.Accept()
	}
	return conn, err
}

// Addr returns the address the listener bound to, which carries the port the
// kernel picked when the server was started with -addr=:0.
func (l *TCPListener) Addr() net.Addr {
	if l.rawconn {
		return l.rawlistener.Addr()
	}
	return l.stdlistener.Addr()
}

func (l *TCPListener) Close() error {
	if l.rawconn {
		return l.rawlistener.Close()
	}
	return l.stdlistener.Close()
}

// newRawConn makes a pooled connection. It starts out closed: it holds no
// socket until [rawsock.Listener.AcceptConn] fills it.
func (l *TCPListener) newRawConn() any { return &rawConn{src: l, closed: true} }

type rawConn struct {
	rawsock.Conn
	src *TCPListener
	// closed guards against a second Close: [rawsock.Conn.Close] is a bare
	// syscall.Close, so closing twice would close whichever file descriptor the
	// process has since been handed in its place.
	closed bool
}

func (rawConn *rawConn) Close() error {
	if rawConn.closed {
		return nil
	}
	rawConn.closed = true
	err := rawConn.Conn.Close()
	rawConn.src.rawconnpool.Put(rawConn)
	return err
}
