package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Hello{Version: Version, Client: "test"}
	if err := WriteJSON(&buf, MsgHello, in); err != nil {
		t.Fatal(err)
	}
	var out Hello
	if err := ReadJSON(&buf, MsgHello, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v want %+v", out, in)
	}
}

func TestWrongType(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteJSON(&buf, MsgHelloAck, HelloAck{})
	if err := ReadJSON(&buf, MsgHello, &Hello{}); err == nil {
		t.Fatal("expected type mismatch error")
	}
}

func TestRemoteError(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteJSON(&buf, MsgError, Error{Message: "denied"})
	err := ReadJSON(&buf, MsgHelloAck, &HelloAck{})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("denied")) {
		t.Fatalf("expected remote error, got %v", err)
	}
}

func TestOversizeFrameRejected(t *testing.T) {
	var hdr [5]byte
	hdr[0] = byte(MsgHello)
	binary.BigEndian.PutUint32(hdr[1:], MaxFrame+1)
	if _, _, err := ReadFrame(bytes.NewReader(hdr[:])); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
	if err := WriteFrame(&bytes.Buffer{}, MsgHello, make([]byte, MaxFrame+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("write: got %v", err)
	}
}

func TestTruncatedFrame(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, MsgHello, []byte("hello"))
	b := buf.Bytes()[:buf.Len()-2]
	if _, _, err := ReadFrame(bytes.NewReader(b)); err == nil {
		t.Fatal("expected error on truncated frame")
	}
}
