package files

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"rmt-desk/internal/proto"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Put uploads size bytes from r as name. c is a fresh files stream.
func Put(c net.Conn, name string, size int64, r io.Reader) (proto.FileDone, error) {
	var done proto.FileDone
	if err := proto.WriteJSON(c, proto.MsgFileReq, proto.FileReq{Op: "put", Name: name, Size: size}); err != nil {
		return done, err
	}
	var ack proto.FileAck
	if err := proto.ReadJSON(c, proto.MsgFileAck, &ack); err != nil {
		return done, err
	}
	sum, n, err := sendData(c, io.LimitReader(r, size))
	if err != nil {
		return done, err
	}
	if n != size {
		return done, fmt.Errorf("source ended early: %d of %d bytes", n, size)
	}
	if err := proto.WriteJSON(c, proto.MsgFileEnd, proto.FileEnd{SHA256: sum}); err != nil {
		return done, err
	}
	if err := proto.ReadJSON(c, proto.MsgFileDone, &done); err != nil {
		return done, err
	}
	if done.SHA256 != sum {
		return done, errors.New("host reported a different checksum")
	}
	return done, nil
}

// List returns the host's exchange folder contents.
func List(c net.Conn) (proto.FileList, error) {
	var l proto.FileList
	if err := proto.WriteJSON(c, proto.MsgFileReq, proto.FileReq{Op: "list"}); err != nil {
		return l, err
	}
	err := proto.ReadJSON(c, proto.MsgFileList, &l)
	return l, err
}

// Get starts a download. onStart is called with the file's name and size
// before any data is written to w (e.g. to set HTTP headers). The checksum is
// verified at the end; on mismatch an error is returned and the caller must
// discard what was written.
func Get(c net.Conn, name string, onStart func(proto.FileAck), w io.Writer) (int64, error) {
	if err := proto.WriteJSON(c, proto.MsgFileReq, proto.FileReq{Op: "get", Name: name}); err != nil {
		return 0, err
	}
	var ack proto.FileAck
	if err := proto.ReadJSON(c, proto.MsgFileAck, &ack); err != nil {
		return 0, err
	}
	if onStart != nil {
		onStart(ack)
	}
	h := sha256.New()
	mw := io.MultiWriter(w, h)
	var n int64
	for {
		t, b, err := proto.ReadFrame(c)
		if err != nil {
			return n, err
		}
		switch t {
		case proto.MsgFileData:
			if n += int64(len(b)); n > ack.Size {
				return n, errors.New("host sent more than the declared size")
			}
			if _, err := mw.Write(b); err != nil {
				return n, err
			}
		case proto.MsgFileEnd:
			var end proto.FileEnd
			if err := json.Unmarshal(b, &end); err != nil {
				return n, err
			}
			if n != ack.Size {
				return n, fmt.Errorf("size mismatch: got %d of %d bytes", n, ack.Size)
			}
			if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), end.SHA256) {
				return n, errors.New("checksum mismatch: download corrupted")
			}
			return n, nil
		case proto.MsgError:
			var e proto.Error
			_ = json.Unmarshal(b, &e)
			return n, &proto.RemoteError{Err: e}
		default:
			return n, fmt.Errorf("unexpected message %d", t)
		}
	}
}
