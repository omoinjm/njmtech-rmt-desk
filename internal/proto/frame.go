// Package proto defines the rmt wire protocol.
//
// Every message is a frame: [type:1][length:4 big-endian][payload:length].
// Control messages carry JSON payloads; bulk data (video tiles, file chunks)
// carries raw bytes. Transport security is provided by WireGuard (tailnet).
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is bumped on any incompatible wire change.
const Version = 1

// MaxFrame bounds payload size so a peer cannot force huge allocations.
const MaxFrame = 16 << 20 // 16 MiB (a 4K JPEG keyframe fits comfortably)

type MsgType uint8

const (
	MsgHello    MsgType = 1 // viewer -> host: Hello
	MsgHelloAck MsgType = 2 // host -> viewer: HelloAck
	MsgError    MsgType = 3 // either: Error
	MsgOpen     MsgType = 4 // first frame on every yamux stream: StreamOpen

	MsgVideoReq   MsgType = 10 // viewer -> host (video stream): VideoReq
	MsgVideoInfo  MsgType = 11 // host -> viewer (video stream): VideoInfo
	MsgVideoFrame MsgType = 12 // host -> viewer (video stream): binary tiles frame
	MsgInput      MsgType = 13 // viewer -> host (input stream): input.Event JSON
)

var ErrFrameTooLarge = errors.New("proto: frame exceeds MaxFrame")

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, t MsgType, payload []byte) error {
	if len(payload) > MaxFrame {
		return ErrFrameTooLarge
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (MsgType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return MsgType(hdr[0]), buf, nil
}

// WriteJSON marshals v and writes it as a frame of type t.
func WriteJSON(w io.Writer, t MsgType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, t, b)
}

// ReadJSON reads a frame, requires type want, and unmarshals into v.
// A MsgError frame is returned as an error.
func ReadJSON(r io.Reader, want MsgType, v any) error {
	t, b, err := ReadFrame(r)
	if err != nil {
		return err
	}
	if t == MsgError {
		var e Error
		_ = json.Unmarshal(b, &e)
		return fmt.Errorf("remote error: %s", e.Message)
	}
	if t != want {
		return fmt.Errorf("proto: got message type %d, want %d", t, want)
	}
	return json.Unmarshal(b, v)
}

// Hello is the viewer's opening message.
type Hello struct {
	Version  int    `json:"version"`
	Client   string `json:"client"`             // e.g. "rmt/0.1.0"
	Password string `json:"password,omitempty"` // unattended access secret (phase 3)
}

// HelloAck is the host's reply on successful admission.
type HelloAck struct {
	Version  int      `json:"version"`
	Host     string   `json:"host"`
	OS       string   `json:"os"`
	Granted  []string `json:"granted"` // permissions: "view","input","clipboard","files"
	Identity string   `json:"identity"`
}

type Error struct {
	Message string `json:"message"`
}

// Stream kinds, sent in StreamOpen as the first frame of each yamux stream.
const (
	StreamControl   = "control"
	StreamVideo     = "video"
	StreamInput     = "input"
	StreamClipboard = "clipboard"
	StreamFiles     = "files"
)

type StreamOpen struct {
	Kind string `json:"kind"`
}

// VideoReq starts or reconfigures the video stream. Sending it again on the
// same stream switches display / quality and forces a keyframe.
type VideoReq struct {
	Display int `json:"display"`
	FPS     int `json:"fps"`     // 1-30
	Quality int `json:"quality"` // JPEG 10-95
}

// VideoInfo is sent by the host whenever the display set or selection changes.
type VideoInfo struct {
	Displays []DisplayInfo `json:"displays"`
	Current  int           `json:"current"`
}

type DisplayInfo struct {
	Index  int `json:"index"`
	Width  int `json:"width"`
	Height int `json:"height"`
}
