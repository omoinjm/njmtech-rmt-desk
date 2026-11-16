// Package h264 encodes screen frames to H.264 with Cisco's OpenH264,
// loaded at runtime (no cgo) from a binary the user downloads from Cisco.
//
// Why runtime loading: Cisco pays the H.264 patent royalties for its
// prebuilt binary only when each user downloads it from Cisco; bundling or
// building it ourselves would not be covered. `rmtd codec install` fetches
// it over HTTPS and verifies a pinned SHA-256, and Load re-verifies the file
// before every dlopen.
//
// "OpenH264 Video Codec provided by Cisco Systems, Inc."
package h264

import (
	"fmt"
	"unsafe"
)

// Mirrors of the OpenH264 2.6.0 C structs (codec_app_def.h). Offsets and
// sizes were taken from the compiler (offsetof/sizeof against the real
// headers, LP64) and are asserted in checkABI, so a mismatch disables H.264
// instead of corrupting memory. Only 64-bit platforms are supported.

type encParamBase struct {
	UsageType    int32
	Width        int32
	Height       int32
	Bitrate      int32
	RCMode       int32
	MaxFrameRate float32
}

type sourcePicture struct {
	ColorFormat int32
	Stride      [4]int32
	Data        [4]*byte
	Width       int32
	Height      int32
	Timestamp   int64
	PsnrY       bool
	PsnrU       bool
	PsnrV       bool
}

type layerBSInfo struct {
	TemporalID uint8
	SpatialID  uint8
	QualityID  uint8
	FrameType  int32
	LayerType  uint8
	SubSeqID   int32
	NalCount   int32
	NalLen     uintptr // int*
	BsBuf      uintptr // unsigned char*
	Psnr       [3]float32
}

type frameBSInfo struct {
	LayerNum  int32
	Layers    [128]layerBSInfo
	FrameType int32
	FrameSize int32
	Timestamp int64
}

const (
	usageScreenContent = 1  // SCREEN_CONTENT_REAL_TIME
	rcBitrateMode      = 1  // RC_BITRATE_MODE
	formatI420         = 23 // videoFormatI420
	frameIDR           = 1
	frameI             = 2
	frameSkip          = 4
	optDataFormat      = 0
	optFrameRate       = 4
	optBitrate         = 5
	optTraceLevel      = 25
	logQuiet           = 0
	spatialLayerAll    = 4

	// ISVCEncoderVtbl slot indexes.
	vInitialize      = 0
	vUninitialize    = 3
	vEncodeFrame     = 4
	vForceIntraFrame = 6
	vSetOption       = 7
)

type bitrateInfo struct {
	Layer   int32
	Bitrate int32
}

func checkABI() error {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return fmt.Errorf("h264: only 64-bit platforms are supported")
	}
	checks := []struct {
		name      string
		got, want uintptr
	}{
		{"sizeof SEncParamBase", unsafe.Sizeof(encParamBase{}), 24},
		{"SEncParamBase.fMaxFrameRate", unsafe.Offsetof(encParamBase{}.MaxFrameRate), 20},
		{"sizeof SSourcePicture", unsafe.Sizeof(sourcePicture{}), 80},
		{"SSourcePicture.iStride", unsafe.Offsetof(sourcePicture{}.Stride), 4},
		{"SSourcePicture.pData", unsafe.Offsetof(sourcePicture{}.Data), 24},
		{"SSourcePicture.iPicWidth", unsafe.Offsetof(sourcePicture{}.Width), 56},
		{"SSourcePicture.uiTimeStamp", unsafe.Offsetof(sourcePicture{}.Timestamp), 64},
		{"SSourcePicture.bPsnrY", unsafe.Offsetof(sourcePicture{}.PsnrY), 72},
		{"sizeof SLayerBSInfo", unsafe.Sizeof(layerBSInfo{}), 56},
		{"SLayerBSInfo.eFrameType", unsafe.Offsetof(layerBSInfo{}.FrameType), 4},
		{"SLayerBSInfo.uiLayerType", unsafe.Offsetof(layerBSInfo{}.LayerType), 8},
		{"SLayerBSInfo.iNalCount", unsafe.Offsetof(layerBSInfo{}.NalCount), 16},
		{"SLayerBSInfo.pNalLengthInByte", unsafe.Offsetof(layerBSInfo{}.NalLen), 24},
		{"SLayerBSInfo.pBsBuf", unsafe.Offsetof(layerBSInfo{}.BsBuf), 32},
		{"sizeof SFrameBSInfo", unsafe.Sizeof(frameBSInfo{}), 7192},
		{"SFrameBSInfo.sLayerInfo", unsafe.Offsetof(frameBSInfo{}.Layers), 8},
		{"SFrameBSInfo.eFrameType", unsafe.Offsetof(frameBSInfo{}.FrameType), 7176},
		{"SFrameBSInfo.uiTimeStamp", unsafe.Offsetof(frameBSInfo{}.Timestamp), 7184},
		{"sizeof SBitrateInfo", unsafe.Sizeof(bitrateInfo{}), 8},
	}
	for _, c := range checks {
		if c.got != c.want {
			return fmt.Errorf("h264: ABI mismatch: %s = %d, want %d", c.name, c.got, c.want)
		}
	}
	return nil
}

// cptr converts an address returned by C (memory the Go GC doesn't manage)
// to an unsafe.Pointer without tripping vet's uintptr checks.
func cptr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }
