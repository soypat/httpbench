package httpbench

import (
	"errors"
	"flag"
	"net"
	"net/netip"
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
	flag.IntVar(&flags.RequestBufferSize, "sz-req", 256, "Request header buffer size")
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
	flags.Port = addrp.Port()
	if flags.Port == 0 {
		flags.Port = 8080
	}
	return err
}

func (flags *Flags) ServerInfo() string {
	return "Running server on http://localhost:" + strconv.Itoa(int(flags.Port))
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
	return l, nil
}

func (l *TCPListener) Accept() (conn net.Conn, err error) {
	if l.rawconn {
		rconn := l.rawconnpool.Get().(*rawConn)
		conn = rconn
		err = l.rawlistener.AcceptConn(&rconn.Conn)
	} else {
		conn, err = l.stdlistener.Accept()
	}
	return conn, err
}

func (l *TCPListener) newRawConn() any { return &rawConn{src: l} }

type rawConn struct {
	rawsock.Conn
	src *TCPListener
}

func (rawConn *rawConn) Close() error {
	rawConn.src.rawconnpool.Put(rawConn)
	return nil
}
