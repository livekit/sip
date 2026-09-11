package integration

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var debugSignalProxy = os.Getenv("DEBUG_SIGNAL_PROXY") != ""

// signalProxy sits in front of the LiveKit signal connection so a test can break
// it. Only the SIP service is pointed at it: media uses separate ports and the
// test's own LiveKit clients dial the server directly, so cutting the proxy takes
// out signaling and nothing else.
type signalProxy struct {
	ln       net.Listener
	upstream string

	// blockResume rejects reconnects that ask the server to resume the existing
	// session. The SDK escalates to a full reconnect once a resume fails, which
	// is the path worth testing.
	blockResume    atomic.Bool
	resumesBlocked atomic.Int32

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newSignalProxy(t testing.TB, upstreamWsURL string) *signalProxy {
	u, err := url.Parse(upstreamWsURL)
	require.NoError(t, err)

	// Resolve once, over IPv4. The harness hands us a "localhost:port" URL, and
	// leaving that to be resolved per dial makes every reconnect race the
	// dual-stack fallback.
	upstream, err := net.ResolveTCPAddr("tcp4", u.Host)
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	p := &signalProxy{
		ln:       ln,
		upstream: upstream.String(),
		conns:    make(map[net.Conn]struct{}),
	}
	t.Cleanup(func() {
		_ = ln.Close()
		p.Cut()
	})
	go p.serve()

	t.Log("Signal proxy:", p.URL(), "->", p.upstream)
	return p
}

// URL is what the SIP service should use as its ws_url.
func (p *signalProxy) URL() string {
	return "ws://" + p.ln.Addr().String()
}

// Cut drops every open connection the way an unplugged network would, and reports
// how many it dropped. A proxied session holds two, one to each side.
func (p *signalProxy) Cut() int {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	clear(p.conns)
	p.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
	return len(conns)
}

func (p *signalProxy) BlockResume(v bool) {
	p.blockResume.Store(v)
}

func (p *signalProxy) ResumesBlocked() int {
	return int(p.resumesBlocked.Load())
}

func (p *signalProxy) Conns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// debugf traces connections when DEBUG_SIGNAL_PROXY is set. It writes to stderr
// rather than to testing.TB because connection goroutines can outlive the subtest
// that started them.
func (p *signalProxy) debugf(format string, args ...any) {
	if !debugSignalProxy {
		return
	}
	fmt.Fprintf(os.Stderr, "signal proxy %s: %s\n",
		time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

func (p *signalProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *signalProxy) handle(client net.Conn) {
	defer client.Close()

	// The websocket handshake is a plain HTTP request, and its first line carries
	// the whole query string. That is enough to tell a resume from a fresh join.
	br := bufio.NewReader(client)
	reqLine, err := br.ReadString('\n')
	if err != nil {
		return
	}
	if p.blockResume.Load() && strings.Contains(reqLine, "reconnect=1") {
		p.resumesBlocked.Add(1)
		p.debugf("blocked resume: %.100q", reqLine)
		return
	}

	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		p.debugf("upstream dial failed: %v", err)
		return
	}
	p.debugf("relaying: %.100q", reqLine)
	defer server.Close()

	p.add(client, server)
	defer p.remove(client, server)

	// The request line was consumed above, so replay it before relaying the rest.
	if _, err = io.WriteString(server, reqLine); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(server, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, server); done <- struct{}{} }()
	<-done
}

func (p *signalProxy) add(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range conns {
		p.conns[c] = struct{}{}
	}
}

func (p *signalProxy) remove(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range conns {
		delete(p.conns, c)
	}
}
