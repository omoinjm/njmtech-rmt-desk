package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"rmt-desk/internal/proto"
)

func TestSanitizeName(t *testing.T) {
	ok := map[string]string{
		"report.pdf":             "report.pdf",
		"../../etc/passwd":       "passwd",
		`..\..\Windows\win.ini`:  "win.ini",
		"/abs/path/x.txt":        "x.txt",
		".bashrc":                "bashrc",
		"a<b>c:d|e?f*.txt":       "a_b_c_d_e_f_.txt",
		"CON.txt":                "_CON.txt",
		"nul":                    "_nul",
		"trailing. ":             "trailing",
		"tab\there.txt":          "tab_here.txt",
		"naïve résumé.docx":      "naïve résumé.docx",
		strings.Repeat("a", 300): strings.Repeat("a", 200),
	}
	for in, want := range ok {
		got, err := SanitizeName(in)
		if err != nil || got != want {
			t.Errorf("SanitizeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".", "..", "/", "a/..", " . ", "dir/"} {
		if got, err := SanitizeName(bad); err == nil {
			t.Errorf("SanitizeName(%q) = %q, want error", bad, got)
		}
	}
}

// serve runs one Exchange request over an in-memory pipe.
func serve(t *testing.T, x *Exchange) net.Conn {
	a, b := net.Pipe()
	go x.Serve(context.Background(), a)
	t.Cleanup(func() { b.Close() })
	return b
}

func newEx(t *testing.T) *Exchange {
	return &Exchange{Dir: filepath.Join(t.TempDir(), "xchg")}
}

func TestPutListGet(t *testing.T) {
	x := newEx(t)
	data := make([]byte, 3*ChunkSize+123)
	rand.Read(data)

	done, err := Put(serve(t, x), "../evil/data.bin", int64(len(data)), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if done.Name != "data.bin" || done.Size != int64(len(data)) {
		t.Fatalf("done %+v", done)
	}
	// Second upload of the same name must not overwrite.
	done2, err := Put(serve(t, x), "data.bin", 5, strings.NewReader("hello"))
	if err != nil || done2.Name != "data (1).bin" {
		t.Fatalf("second put: %+v %v", done2, err)
	}
	if b, _ := os.ReadFile(filepath.Join(x.Dir, "data.bin")); !bytes.Equal(b, data) {
		t.Fatal("original overwritten or corrupted")
	}

	l, err := List(serve(t, x))
	if err != nil || len(l.Files) != 2 {
		t.Fatalf("list: %+v %v", l, err)
	}

	var got bytes.Buffer
	var ack proto.FileAck
	n, err := Get(serve(t, x), "data.bin", func(a proto.FileAck) { ack = a }, &got)
	if err != nil || n != int64(len(data)) || ack.Size != n || !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("get: n=%d err=%v", n, err)
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(x.Dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestGetRejectsTraversalAndSymlinks(t *testing.T) {
	x := newEx(t)
	os.MkdirAll(x.Dir, 0o700)
	secret := filepath.Join(filepath.Dir(x.Dir), "secret.txt")
	os.WriteFile(secret, []byte("top secret"), 0o600)

	for _, name := range []string{"../secret.txt", "/etc/passwd", ".hidden", ""} {
		if _, err := Get(serve(t, x), name, nil, &bytes.Buffer{}); err == nil {
			t.Errorf("get %q should fail", name)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(secret, filepath.Join(x.Dir, "link.txt")); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := Get(serve(t, x), "link.txt", nil, &buf); err == nil || buf.Len() > 0 {
			t.Fatalf("symlink escape: err=%v data=%q", err, buf.String())
		}
		l, _ := List(serve(t, x))
		if len(l.Files) != 0 {
			t.Fatalf("symlink listed: %+v", l.Files)
		}
	}
}

// rawPut drives the protocol by hand to simulate a misbehaving sender.
func rawPut(t *testing.T, x *Exchange, declared int64, payload []byte, sum string) error {
	c := serve(t, x)
	proto.WriteJSON(c, proto.MsgFileReq, proto.FileReq{Op: "put", Name: "f.bin", Size: declared})
	var ack proto.FileAck
	if err := proto.ReadJSON(c, proto.MsgFileAck, &ack); err != nil {
		return err
	}
	if err := proto.WriteFrame(c, proto.MsgFileData, payload); err != nil {
		return err
	}
	proto.WriteJSON(c, proto.MsgFileEnd, proto.FileEnd{SHA256: sum})
	var done proto.FileDone
	return proto.ReadJSON(c, proto.MsgFileDone, &done)
}

func TestPutRejectsCorruption(t *testing.T) {
	x := newEx(t)
	if err := rawPut(t, x, 5, []byte("hello"), strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum: %v", err)
	}
	if err := rawPut(t, x, 3, []byte("hello"), ""); err == nil {
		t.Fatal("oversend accepted")
	}
	if err := rawPut(t, x, 10, []byte("hello"), ""); err == nil {
		t.Fatal("undersend accepted")
	}
	ents, _ := os.ReadDir(x.Dir)
	if len(ents) != 0 {
		t.Fatalf("failed uploads left files: %v", ents)
	}
}

func TestPutSizeLimit(t *testing.T) {
	x := newEx(t)
	x.MaxBytes = 10
	if _, err := Put(serve(t, x), "big.bin", 11, bytes.NewReader(make([]byte, 11))); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("limit: %v", err)
	}
}
