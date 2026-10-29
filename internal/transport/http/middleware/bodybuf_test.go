package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBufferClassesRoundUpAndStopAtTheLargest(t *testing.T) {
	if b := takeBuffer(10); cap(b) != 64<<10 {
		t.Fatalf("a tiny body takes the smallest class, got cap %d", cap(b))
	}
	if b := takeBuffer(5 << 20); cap(b) != 8<<20 {
		t.Fatalf("5 MiB rounds up to the 8 MiB class, got cap %d", cap(b))
	}
	if b := takeBuffer(9 << 20); cap(b) != 9<<20 {
		t.Fatalf("past the largest class the size is taken as asked, got cap %d", cap(b))
	}
	giveBuffer(make([]byte, 0, 9<<20)) // dropped, not pooled
	giveBuffer(nil)                    // nothing to drop
}

func TestReadBodyGrowsPastAWrongHintAndFitsARightOne(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 200<<10)
	state := &State{}
	got, err := readBody(bytes.NewReader(body), 1, 1<<20, state)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("a body past its hint is still read whole: err=%v len=%d", err, len(got))
	}
	got, err = readBody(bytes.NewReader(body), int64(len(body)), 1<<20, state)
	if err != nil || !bytes.Equal(got, body) || cap(got) != 256<<10 {
		t.Fatalf("a right hint reads into one buffer of its class: err=%v cap=%d", err, cap(got))
	}
	if _, err := readBody(bytes.NewReader(body), 1<<40, 1<<20, state); err != nil {
		t.Fatalf("a huge hint is capped at the limit, not allocated: %v", err)
	}
	limited := http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(bytes.NewReader(body)), 1024)
	if _, err := readBody(limited, int64(len(body)), 1024, &State{}); err == nil {
		t.Fatal("a body over the limit is an error")
	}
}

func TestABodyReaderRefusesOnceClosed(t *testing.T) {
	r := &bodyReader{reader: bytes.NewReader([]byte("abc"))}
	p := make([]byte, 2)
	if n, _ := r.Read(p); n != 2 {
		t.Fatalf("read %d", n)
	}
	done := make(chan struct{})
	go func() { // a Read racing Close must serialise on the lock, which -race checks
		for {
			if _, err := r.Read(p); err != nil {
				close(done)
				return
			}
		}
	}()
	_ = r.Close()
	<-done
	if n, err := r.Read(p); n != 0 || !errors.Is(err, http.ErrBodyReadAfterClose) {
		t.Fatalf("after Close: n=%d err=%v", n, err)
	}
}

func TestReleaseClosesEveryReaderAndForgetsTheBuffers(t *testing.T) {
	state := &State{Raw: []byte("{}")}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	restoreBody(r, state, state.Raw)
	first := r.Body
	restoreBody(r, state, []byte(`{"a":1}`))
	state.own(takeBuffer(1))
	state.release()
	for _, body := range []interface{ Read([]byte) (int, error) }{first, r.Body} {
		if _, err := body.Read(make([]byte, 1)); !errors.Is(err, http.ErrBodyReadAfterClose) {
			t.Fatalf("a reader left open after release: %v", err)
		}
	}
	if state.buffers != nil || state.readers != nil || state.Raw != nil {
		t.Fatal("release keeps a reference to the request's bytes")
	}
}
