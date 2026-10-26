package protocol

import (
	"encoding/binary"
	"fmt"
)

// Display streams carry a compositor's output. The guest picks the size, so
// every stream opens with [FrameDisplayGeometry]; after it, frames are
// full-width bands, which keeps one frame under [MaxFramePayload] however
// large the output is. A band is sent only when its pixels changed, so a
// still screen costs nothing but the geometry, and the app keeps its own copy
// of the last full frame.
const (
	// FrameDisplayGeometry payload: [width u16][height u16][format u8].
	FrameDisplayGeometry byte = 3
	// FrameDisplayBand payload: [y u16][height u16][stride u32] then
	// height*stride bytes of pixels in the geometry's format.
	FrameDisplayBand byte = 4
)

// DisplayFormatRGBA is R,G,B,A per pixel: the byte order Android's ARGB_8888
// bitmap has in memory, so the app can copy pixels without converting them.
const DisplayFormatRGBA byte = 1

const displayGeometryLen = 5
const displayBandHeader = 8

// EncodeDisplayGeometry returns the FrameDisplayGeometry payload.
func EncodeDisplayGeometry(width, height uint16) []byte {
	b := make([]byte, displayGeometryLen)
	binary.BigEndian.PutUint16(b[0:], width)
	binary.BigEndian.PutUint16(b[2:], height)
	b[4] = DisplayFormatRGBA
	return b
}

// DecodeDisplayGeometry parses the FrameDisplayGeometry payload.
func DecodeDisplayGeometry(b []byte) (width, height uint16, format byte, err error) {
	if len(b) != displayGeometryLen {
		return 0, 0, 0, fmt.Errorf("geometry payload is %d bytes, want %d", len(b), displayGeometryLen)
	}
	return binary.BigEndian.Uint16(b[0:]), binary.BigEndian.Uint16(b[2:]), b[4], nil
}

// EncodeDisplayBand returns the FrameDisplayBand payload for one band of
// pixels: height rows of stride bytes starting at y.
func EncodeDisplayBand(y, height uint16, stride uint32, pixels []byte) ([]byte, error) {
	if len(pixels) != int(height)*int(stride) {
		return nil, fmt.Errorf("band is %d bytes, want %d", len(pixels), int(height)*int(stride))
	}
	b := make([]byte, displayBandHeader+len(pixels))
	binary.BigEndian.PutUint16(b[0:], y)
	binary.BigEndian.PutUint16(b[2:], height)
	binary.BigEndian.PutUint32(b[4:], stride)
	copy(b[displayBandHeader:], pixels)
	return b, nil
}

// DecodeDisplayBand parses the FrameDisplayBand payload.
func DecodeDisplayBand(b []byte) (y, height uint16, stride uint32, pixels []byte, err error) {
	if len(b) < displayBandHeader {
		return 0, 0, 0, nil, fmt.Errorf("band payload is %d bytes, want at least %d", len(b), displayBandHeader)
	}
	y = binary.BigEndian.Uint16(b[0:])
	height = binary.BigEndian.Uint16(b[2:])
	stride = binary.BigEndian.Uint32(b[4:])
	pixels = b[displayBandHeader:]
	if want := int(height) * int(stride); len(pixels) != want {
		return 0, 0, 0, nil, fmt.Errorf("band carries %d bytes, want %d", len(pixels), want)
	}
	return y, height, stride, pixels, nil
}

// MaxDisplayBandHeight is the tallest band of a width-pixel RGBA frame that
// still fits one frame, so a sender can split a full-size frame safely.
func MaxDisplayBandHeight(width int) int {
	if width <= 0 {
		return 0
	}
	rows := (MaxFramePayload - displayBandHeader) / (width * 4)
	return max(rows, 1)
}
