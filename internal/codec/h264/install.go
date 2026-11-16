package h264

import (
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// Release pins one Cisco OpenH264 binary.
type Release struct {
	URL       string // https download (bzip2)
	BZ2SHA256 string
	LibName   string
	LibSHA256 string // of the decompressed library, re-checked before every load
}

const ciscoBase = "https://ciscobinary.openh264.org/"

// releases: OpenH264 2.6.0, hashes recorded from ciscobinary.openh264.org.
var releases = map[string]Release{
	"linux/amd64": {ciscoBase + "libopenh264-2.6.0-linux64.8.so.bz2",
		"27ab53323c110b76214c1c72222f459d17febbcd1e252136cadc292b0308d75b",
		"libopenh264-2.6.0-linux64.8.so",
		"2f0cde7c6a6abcf5cae76942894ea42897fa677bce4ed6c91a24dd1b041d5f04"},
	"linux/arm64": {ciscoBase + "libopenh264-2.6.0-linux-arm64.8.so.bz2",
		"a78aea7970150f46bcd3bb7994c9e6dd90bd7a9ea785920f5a73f6964e3fcda7",
		"libopenh264-2.6.0-linux-arm64.8.so",
		"12e7b33623667cdab0e575170c147b1b36eadb77d0d2aa7ceb5afd3e58902140"},
	"windows/amd64": {ciscoBase + "openh264-2.6.0-win64.dll.bz2",
		"dab5f2a872777f9a58b69bfa9fbcf20d9f82f2d6ec91383fd70bff49bd34ac9f",
		"openh264-2.6.0-win64.dll",
		"2076cb5675ec6c1a4c70e7a2a322552f547b6eeed649d6dfcd9e02a543b24691"},
	"windows/arm64": {ciscoBase + "openh264-2.6.0-win-arm64.dll.bz2",
		"6b57d3ecd06bd80b5f3ee1abbb31d48798ca6d89ef1bc98cf2e6d1ebddf5cf45",
		"openh264-2.6.0-win-arm64.dll",
		"fb75103938f4f47d119b983e06334df41a803bc72fb5c46e3623f6fea5782732"},
	"darwin/amd64": {ciscoBase + "libopenh264-2.6.0-mac-x64.dylib.bz2",
		"38b2ed6d1d45b6a3e408c734173f2d67ab44a10d0e154ff3489b89877cd60e7e",
		"libopenh264-2.6.0-mac-x64.dylib",
		"e3dc8bc01fe69363f61fd3c02fd27798537a585eadd38cd808f303d1ee505a19"},
	"darwin/arm64": {ciscoBase + "libopenh264-2.6.0-mac-arm64.dylib.bz2",
		"6db362ee5abdab572311aeadb96d3f44b0617d9a4a4b9f4db4cb5ac4d968da71",
		"libopenh264-2.6.0-mac-arm64.dylib",
		"052e98bfcf7a9167d22f3bbb3f5988ef79065591f36af8b52924b22b13624551"},
}

func currentRelease() (Release, error) {
	r, ok := releases[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return r, fmt.Errorf("h264: no OpenH264 build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return r, nil
}

// LibPath is where Install puts the library inside dir.
func LibPath(dir string) (string, error) {
	r, err := currentRelease()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, r.LibName), nil
}

// Notice must be shown to users of the codec (Cisco license terms).
const Notice = "OpenH264 Video Codec provided by Cisco Systems, Inc. Downloaded from Cisco; see https://www.openh264.org/BINARY_LICENSE.txt"

// Install downloads the platform's library from Cisco into dir, verifying
// both the download and the decompressed library against pinned hashes.
func Install(ctx context.Context, dir string) (string, error) {
	r, err := currentRelease()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", r.URL, nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("download %s: %s", r.URL, res.Status)
	}
	bz, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(bz); hex.EncodeToString(sum[:]) != r.BZ2SHA256 {
		return "", fmt.Errorf("download checksum mismatch for %s; refusing to install", r.URL)
	}
	tmp, err := os.CreateTemp(dir, ".openh264-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(bzip2.NewReader(bytesReader(bz)), 64<<20)); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != r.LibSHA256 {
		return "", fmt.Errorf("decompressed library checksum mismatch; refusing to install")
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, r.LibName)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}
