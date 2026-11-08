// Package deadaddr provides a loopback address that resets every connection
// for the life of the process, for tests of a supervisor, engine or proxy that
// is gone. A caller sees a reset (ECONNRESET or EOF), not a refusal
// (ECONNREFUSED); the code under test treats every transport error alike.
//
// A port that is bound and then closed is not such an address. It is free
// only until the kernel hands it to the next listener -- any httptest server
// in the same test binary, or in another package's binary that make test runs
// beside it -- and from then on the "dead" peer answers whatever that server
// says. That made the warm-pool tests flaky (#217): a broken Pod read as
// healthy, and requests meant for nothing reached another test's fake.
package deadaddr

import (
	"fmt"
	"net"
	"sync"
)

var (
	once sync.Once
	addr *net.TCPAddr
)

// Addr returns a 127.0.0.1 address whose listener is held for the life of the
// process, so no other listener can take its port, and which resets every
// connection as soon as it is accepted, so every request to it fails. It
// panics if no loopback port can be bound: a test cannot run without one.
func Addr() string {
	return listen().String()
}

// Port returns the port of Addr.
func Port() int {
	return listen().Port
}

func listen() *net.TCPAddr {
	once.Do(func() {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(fmt.Sprintf("deadaddr: bind a loopback port: %v", err))
		}
		addr = l.Addr().(*net.TCPAddr)
		go refuse(l)
	})
	return addr
}

// refuse accepts and resets every connection to l, until l fails.
func refuse(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}
}
