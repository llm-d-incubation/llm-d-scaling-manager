package deadaddr

import (
	"net"
	"net/http"
	"strconv"
	"testing"
)

func TestTheAddressCannotBeTakenByAnotherListener(t *testing.T) {
	// The tests that use it are only as good as this: if another listener can
	// bind the port, the dead peer answers whatever that listener says.
	l, err := net.Listen("tcp", Addr())
	if err == nil {
		_ = l.Close()
		t.Fatalf("%s could be bound by another listener, so a dead peer can answer", Addr())
	}
}

func TestEveryRequestToTheAddressFails(t *testing.T) {
	for i := 0; i < 3; i++ {
		resp, err := http.Get("http://" + Addr() + "/health")
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("request %d to %s answered %s, want it to fail", i, Addr(), resp.Status)
		}
	}
}

func TestPortIsTheAddressPort(t *testing.T) {
	if got, want := net.JoinHostPort("127.0.0.1", strconv.Itoa(Port())), Addr(); got != want {
		t.Fatalf("Port() gives %s, Addr() is %s", got, want)
	}
}
