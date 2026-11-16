// Package files implements file transfer through a single sandboxed
// exchange folder on the host.
//
// The viewer can upload into the folder, list it, and download from it, and
// nothing else: no paths, no subdirectories, no symlinks. Uploads are written
// to an O_EXCL temp file, verified (size + SHA-256) and then hard-linked to a
// fresh name, so they never overwrite an existing file and a failed transfer
// leaves nothing behind.
package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"rmt-desk/internal/proto"
)

const (
	ChunkSize       = 256 << 10
	DefaultMaxBytes = 4 << 30 // 4 GiB per file
	maxNameLen      = 200
)

// DefaultDir is the exchange folder: ~/Downloads/rmt-desk.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Downloads", "rmt-desk"), nil
}

var ErrBadName = errors.New("files: invalid file name")

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// SanitizeName reduces an untrusted name to a safe single path element that
// is valid on Windows, macOS and Linux.
func SanitizeName(name string) (string, error) {
	// Treat both separators as separators on every OS, keep the last element.
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), strings.ContainsRune(`<>:"|?*`, r):
			return '_'
		}
		return r
	}, name)
	name = strings.TrimRight(strings.TrimSpace(name), ". ") // Windows strips these silently
	name = strings.TrimLeft(name, ".")                      // no hidden files / "..": dotfiles become visible
	if name == "" {
		return "", ErrBadName
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if windowsReserved[stem] {
		name = "_" + name
	}
	if len(name) > maxNameLen {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = truncateUTF8(name[:len(name)-len(ext)], maxNameLen-len(ext)) + ext
	}
	return name, nil
}

func truncateUTF8(s string, n int) string {
	for n > 0 && n < len(s) && !utf8Start(s[n]) {
		n--
	}
	if n < len(s) {
		return s[:n]
	}
	return s
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// Exchange serves file operations for one folder.
type Exchange struct {
	Dir      string
	MaxBytes int64
	// OnEvent is called for completed transfers (for logging/audit).
	OnEvent func(op, name string, size int64)
}

func (x *Exchange) max() int64 {
	if x.MaxBytes > 0 {
		return x.MaxBytes
	}
	return DefaultMaxBytes
}

func (x *Exchange) event(op, name string, size int64) {
	if x.OnEvent != nil {
		x.OnEvent(op, name, size)
	}
}

// fail reports err and lets Serve close the stream. The peer may be blocked
// writing data to us (mid-upload), so the error write is time-bounded rather
// than waiting forever; closing the stream then unblocks the peer.
func fail(c net.Conn, err error) {
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Message: err.Error()})
}

// Serve handles one request on a files stream (host side).
func (x *Exchange) Serve(ctx context.Context, c net.Conn) {
	defer c.Close()
	context.AfterFunc(ctx, func() { c.Close() })
	if err := os.MkdirAll(x.Dir, 0o700); err != nil {
		fail(c, fmt.Errorf("exchange folder: %w", err))
		return
	}
	var req proto.FileReq
	if err := proto.ReadJSON(c, proto.MsgFileReq, &req); err != nil {
		return
	}
	var err error
	switch req.Op {
	case "list":
		err = x.list(c)
	case "put":
		err = x.put(c, req)
	case "get":
		err = x.get(c, req)
	default:
		err = fmt.Errorf("unknown operation %q", req.Op)
	}
	if err != nil {
		fail(c, err)
	}
}

func (x *Exchange) list(c net.Conn) error {
	ents, err := os.ReadDir(x.Dir)
	if err != nil {
		return err
	}
	out := proto.FileList{Dir: x.Dir, Files: []proto.FileInfo{}}
	for _, e := range ents {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue // skip dirs, symlinks, temp/hidden files
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out.Files = append(out.Files, proto.FileInfo{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime().Unix()})
	}
	slices.SortFunc(out.Files, func(a, b proto.FileInfo) int { return int(b.ModTime - a.ModTime) })
	return proto.WriteJSON(c, proto.MsgFileList, out)
}

func (x *Exchange) put(c net.Conn, req proto.FileReq) error {
	name, err := SanitizeName(req.Name)
	if err != nil {
		return err
	}
	if req.Size < 0 || req.Size > x.max() {
		return fmt.Errorf("file too large (limit %d MiB)", x.max()>>20)
	}
	tmp, err := os.CreateTemp(x.Dir, ".rmt-*.part") // O_EXCL, 0600
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmp.Name())
		}
	}()
	if err := proto.WriteJSON(c, proto.MsgFileAck, proto.FileAck{Name: name, Size: req.Size}); err != nil {
		return err
	}
	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	var n int64
	var end proto.FileEnd
	for {
		t, b, err := proto.ReadFrame(c)
		if err != nil {
			return err
		}
		if t == proto.MsgFileEnd {
			if err := jsonUnmarshal(b, &end); err != nil {
				return err
			}
			break
		}
		if t != proto.MsgFileData {
			return fmt.Errorf("unexpected message %d", t)
		}
		if n += int64(len(b)); n > req.Size {
			return errors.New("sender exceeded declared size")
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if n != req.Size {
		return fmt.Errorf("size mismatch: got %d, declared %d", n, req.Size)
	}
	if !strings.EqualFold(sum, end.SHA256) {
		return errors.New("checksum mismatch: file corrupted in transit")
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	final, err := commit(tmp.Name(), x.Dir, name)
	if err != nil {
		return err
	}
	committed = true
	x.event("received", final, n)
	return proto.WriteJSON(c, proto.MsgFileDone, proto.FileDone{Name: final, Size: n, SHA256: sum})
}

// commit links tmp to a name that doesn't exist yet: "a.txt", "a (1).txt", ...
func commit(tmp, dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		dst := filepath.Join(dir, cand)
		err := os.Link(tmp, dst) // atomic and fails if dst exists: no overwrite, no TOCTOU
		if err == nil {
			os.Remove(tmp)
			return cand, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		// Filesystem without hard links (FAT/exFAT): best-effort fallback.
		if _, serr := os.Lstat(dst); errors.Is(serr, os.ErrNotExist) {
			if err := os.Rename(tmp, dst); err != nil {
				return "", err
			}
			return cand, nil
		}
	}
	return "", errors.New("could not find a free file name")
}

func (x *Exchange) get(c net.Conn, req proto.FileReq) error {
	name, err := SanitizeName(req.Name)
	if err != nil || name != req.Name || strings.HasPrefix(name, ".") {
		return ErrBadName // only exact names from list are fetchable
	}
	p := filepath.Join(x.Dir, name)
	lfi, err := os.Lstat(p)
	if err != nil {
		return errors.New("no such file")
	}
	if !lfi.Mode().IsRegular() {
		return errors.New("not a regular file") // rejects symlinks out of the sandbox
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !os.SameFile(lfi, fi) { // swapped between Lstat and Open
		return errors.New("file changed during open")
	}
	if err := proto.WriteJSON(c, proto.MsgFileAck, proto.FileAck{Name: name, Size: fi.Size()}); err != nil {
		return err
	}
	sum, n, err := sendData(c, f)
	if err != nil {
		return err
	}
	x.event("sent", name, n)
	return proto.WriteJSON(c, proto.MsgFileEnd, proto.FileEnd{SHA256: sum})
}

func sendData(c net.Conn, r io.Reader) (string, int64, error) {
	h := sha256.New()
	buf := make([]byte, ChunkSize)
	var n int64
	for {
		k, err := io.ReadFull(r, buf)
		if k > 0 {
			h.Write(buf[:k])
			n += int64(k)
			if werr := proto.WriteFrame(c, proto.MsgFileData, buf[:k]); werr != nil {
				return "", n, werr
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return hex.EncodeToString(h.Sum(nil)), n, nil
		}
		if err != nil {
			return "", n, err
		}
	}
}
