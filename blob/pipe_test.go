package blob

import (
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

func TestPipeDeliversWritesInOrderThenEOF(t *testing.T) {
	p := newPipe(2)
	go func() {
		for _, s := range []string{"alpha", "beta", "gamma", "delta"} {
			if _, err := p.Write([]byte(s)); err != nil {
				t.Errorf("Write(%q): %v", s, err)
			}
		}
		p.CloseWrite(nil)
	}()
	got, err := io.ReadAll(iotest.OneByteReader(p))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "alphabetagammadelta" {
		t.Fatalf("read %q", got)
	}
	if n, err := p.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("Read after EOF = %d, %v; want 0, io.EOF", n, err)
	}
}

func TestPipeCopiesWrittenBytes(t *testing.T) {
	p := newPipe(4)
	buf := []byte("first")
	if _, err := p.Write(buf); err != nil {
		t.Fatal(err)
	}
	copy(buf, "xxxxx")
	p.CloseWrite(nil)
	got, err := io.ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("read %q, want the bytes as written", got)
	}
}

func TestPipeCloseWriteErrorFollowsTheData(t *testing.T) {
	p := newPipe(4)
	if _, err := p.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	want := errors.New("compose failed")
	p.CloseWrite(want)
	got, err := io.ReadAll(p)
	if !errors.Is(err, want) {
		t.Fatalf("ReadAll error = %v, want %v", err, want)
	}
	if string(got) != "data" {
		t.Fatalf("read %q before the error, want \"data\"", got)
	}
}

func TestPipeCloseReadUnblocksAndFailsTheWriter(t *testing.T) {
	p := newPipe(1)
	if _, err := p.Write([]byte("fills the only slot")); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := p.Write([]byte("blocks until the reader is gone"))
		errc <- err
	}()
	p.CloseRead()
	if err := <-errc; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("blocked Write returned %v, want io.ErrClosedPipe", err)
	}
	if _, err := p.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Write after CloseRead = %v, want io.ErrClosedPipe", err)
	}
	if _, err := p.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Read after CloseRead = %v, want io.ErrClosedPipe", err)
	}
}

func TestPipeCloseReadIsIdempotent(t *testing.T) {
	p := newPipe(1)
	p.CloseRead()
	p.CloseRead()
	p.CloseWrite(nil)
	if _, err := p.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Read = %v, want io.ErrClosedPipe", err)
	}
}
