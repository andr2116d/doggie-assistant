package bus

import (
	"io"
	"net"
	"sync"
	"testing"
)

// faultProxy forwards TCP connections to target and can break them the ways
// a network does: cut them all at once, or blackhole them, keeping the
// sockets open while nothing gets through, as when a laptop sleeps or a
// tunnel drops without a word.
type faultProxy struct {
	t      *testing.T
	ln     net.Listener
	target string

	mu     sync.Mutex
	conns  []net.Conn
	refuse bool          // new connections are closed at once
	open   chan struct{} // closed while traffic flows
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{t: t, ln: ln, target: target, open: make(chan struct{})}
	close(p.open)
	t.Cleanup(func() { ln.Close(); p.cut() })
	go p.accept()
	return p
}

// URL is the proxy's address for Dial.
func (p *faultProxy) URL() string { return "nats://" + p.ln.Addr().String() }

func (p *faultProxy) accept() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refuse := p.refuse
		p.mu.Unlock()
		if refuse {
			client.Close()
			continue
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go p.pipe(server, client)
		go p.pipe(client, server)
	}
}

// pipe copies from src to dst while traffic flows, and holds what it read
// while blackholed.
func (p *faultProxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		p.mu.Lock()
		open := p.open
		p.mu.Unlock()
		<-open
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				src.Close()
			}
			return
		}
	}
}

// blackhole stops all traffic, leaving every socket open.
func (p *faultProxy) blackhole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.open:
		p.open = make(chan struct{})
	default:
	}
}

// cut closes every connection and refuses new ones until restore.
func (p *faultProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse = true
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

// restore lets traffic and new connections through again.
func (p *faultProxy) restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse = false
	select {
	case <-p.open:
	default:
		close(p.open)
	}
}
