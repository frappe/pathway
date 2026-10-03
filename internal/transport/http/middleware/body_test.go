package middleware

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A client that sends its headers and then drips the body is cut off with a 408 rather than
// holding the request open, and nothing below the body stage runs.
func TestAStalledBodyTimesOut(t *testing.T) {
	previous := bodyReadTimeout
	bodyReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { bodyReadTimeout = previous })

	body, err := newBody(Deps{})
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{}, 1)
	server := httptest.NewServer(body(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached <- struct{}{}
	})))
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\n" +
		"Content-Type: application/json\r\nContent-Length: 100\r\n\r\n{"))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body: %v", err)
	}
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Errorf("status = %d, want 408", resp.StatusCode)
	}
	select {
	case <-reached:
		t.Error("a request whose body never arrived reached the next stage")
	default:
	}
}
