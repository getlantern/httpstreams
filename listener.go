package httpstreams

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/golog"
	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
)

var (
	log = golog.LoggerFor("httpstreams")
)

type ListenOptions struct {
	Addr       string
	Path       string
	TLSConfig  *tls.Config
	QuicConfig *quic.Config
	Handler    *http.ServeMux
}

func ListenH2(options *ListenOptions) (net.Listener, error) {

	addr := options.Addr
	if addr == "" {
		addr = "0.0.0.0:443"
	}
	tcpListener, err := net.Listen("tcp", options.Addr)
	if err != nil {
		return nil, err
	}

	tlsConfig := options.TLSConfig.Clone()
	if len(tlsConfig.NextProtos) > 0 {
		// will be enabled by default if nil, otherwise we check for
		// h2 explicitly specified.
		hasH2 := false
		for _, v := range tlsConfig.NextProtos {
			if v == "h2" {
				hasH2 = true
				break
			}
		}
		if !hasH2 {
			return nil, fmt.Errorf("h2 must be enabled if NextProtos is given in TLSConfig")
		}
	} else {
		tlsConfig.NextProtos = []string{"h2", "http/1.1"}
	}
	tlsListener := tls.NewListener(tcpListener, tlsConfig)

	handler := options.Handler
	if handler == nil {
		handler = http.NewServeMux()
	}

	server := &http.Server{Handler: handler}
	l := &listener{
		addr:         tlsListener.Addr(),
		connections:  make(chan net.Conn, 1000),
		acceptError:  make(chan error, 1),
		closedSignal: make(chan struct{}),
		onClose: func() error {
			return server.Close()
		},
	}

	path := fmt.Sprintf("/%s/", options.Path)
	handler.HandleFunc(path, l.handleUpgrade)

	go func() {
		err := server.Serve(tlsListener)
		if err != nil {
			if !l.isClosed() {
				l.acceptError <- err
			}
		}
	}()

	go l.logStats()

	return l, nil
}

func ListenH3(options *ListenOptions) (net.Listener, error) {
	addr := options.Addr
	if addr == "" {
		addr = "0.0.0.0:443"
	}
	handler := options.Handler
	if handler == nil {
		handler = http.NewServeMux()
	}

	server := http3.Server{
		Handler:    handler,
		Addr:       addr,
		TLSConfig:  options.TLSConfig,
		QuicConfig: options.QuicConfig,
	}

	udpAddr, err := net.ResolveUDPAddr("udp", options.Addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}

	l := &listener{
		addr:         conn.LocalAddr(),
		connections:  make(chan net.Conn, 1000),
		acceptError:  make(chan error, 1),
		closedSignal: make(chan struct{}),
		onClose: func() error {
			return server.Close()
		},
	}

	path := fmt.Sprintf("/%s/", options.Path)
	handler.HandleFunc(path, l.handleUpgrade)

	go func() {
		err := server.Serve(conn)
		if err != nil {
			if !l.isClosed() {
				l.acceptError <- err
			}
		}
	}()

	go l.logStats()

	return l, nil

}

type listener struct {
	numConnections int64

	onClose      func() error
	addr         net.Addr
	connections  chan net.Conn
	acceptError  chan error
	closedSignal chan struct{}
	closeErr     error
	closeOnce    sync.Once
}

// implements net.Listener.Accept
func (l *listener) Accept() (net.Conn, error) {
	select {
	case conn, ok := <-l.connections:
		if !ok {
			return nil, http.ErrServerClosed
		}
		return conn, nil
	case err, ok := <-l.acceptError:
		if !ok {
			return nil, http.ErrServerClosed
		}
		return nil, err
	case <-l.closedSignal:
		return nil, http.ErrServerClosed
	}
}

// implements net.Listener.Close
func (l *listener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closedSignal)
		l.closeErr = l.onClose()
	})
	return l.closeErr
}

func (l *listener) Addr() net.Addr {
	return l.addr
}

func (l *listener) isClosed() bool {
	select {
	case <-l.closedSignal:
		return true
	default:
		return false
	}
}

func (l *listener) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	log.Debugf("handling an upgrade ...\n")

	// check method ... other things?
	log.Debugf("method is %s", r.Method)
	log.Debugf("proto is %s", r.TLS.NegotiatedProtocol)

	atomic.AddInt64(&l.numConnections, 1)
	conn, err := newServerConn(w, r, l.Addr())
	if err != nil {
		log.Errorf("Failed to upgrade h2 connection: %w", err)
		return
	}

	l.connections <- conn
	// wait for it to close ...
	conn.waitForClose()
	atomic.AddInt64(&l.numConnections, -1)
}

func (l *listener) logStats() {
	for {
		select {
		case <-time.After(5 * time.Second):
			if !l.isClosed() {
				log.Debugf("Virtual Connections: %d", atomic.LoadInt64(&l.numConnections))
			}
		case <-l.closedSignal:
			log.Debugf("Virtual Connections: %d", atomic.LoadInt64(&l.numConnections))
			log.Debug("Done logging stats.")
			return
		}
	}
}

func newServerConn(w http.ResponseWriter, r *http.Request, localAddr net.Addr) (*serverConn, error) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("http.ResponseWriter did not support flush")
	}

	remoteAddr, err := net.ResolveTCPAddr("tcp", r.RemoteAddr)
	if err != nil {
		return nil, err
	}

	return &serverConn{
		writer:       w,
		writeFlush:   flusher,
		request:      r,
		closedSignal: make(chan struct{}),
		localAddr:    localAddr,
		remoteAddr:   remoteAddr,
	}, nil
}

type serverConn struct {
	writer       http.ResponseWriter
	writeFlush   http.Flusher
	request      *http.Request
	closedSignal chan struct{}
	closeOnce    sync.Once
	closeErr     error
	localAddr    net.Addr
	remoteAddr   net.Addr
}

func (c *serverConn) waitForClose() {
	<-c.closedSignal
}

// implements net.Conn.Read
func (c *serverConn) Read(b []byte) (int, error) {
	return c.request.Body.Read(b)
}

// implements net.Conn.Write
func (c *serverConn) Write(b []byte) (int, error) {
	n, err := c.writer.Write(b)
	if n > 0 {
		c.writeFlush.Flush()
	}
	return n, err
}

// implements net.Conn.Close
func (c *serverConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closedSignal)
	})
	return c.closeErr
}

// implements net.Conn.LocalAddr
func (c *serverConn) LocalAddr() net.Addr {
	return c.localAddr
}

// implements net.Conn.RemoteAddr
func (c *serverConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *serverConn) SetDeadline(t time.Time) error {
	// not currently supported
	return nil
}

func (c *serverConn) SetReadDeadline(t time.Time) error {
	// not currently supported
	return nil
}

func (c *serverConn) SetWriteDeadline(t time.Time) error {
	// not currently supported
	return nil
}
