package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestHostFSRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHostFS(&buf, FrameHostFSRequest, 7, HostFSRequest{Op: FileWrite, Handle: 3, Offset: 1 << 40}, []byte("bytes")); err != nil {
		t.Fatal(err)
	}
	if err := WriteHostFS(&buf, FrameHostFSReply, 8, HostFSReply{Errno: "ENOENT", Error: "gone"}, nil); err != nil {
		t.Fatal(err)
	}
	var req HostFSRequest
	typ, id, data, err := ReadHostFS(&buf, &req)
	if err != nil || typ != FrameHostFSRequest || id != 7 || string(data) != "bytes" {
		t.Fatalf("request = %#x %d %q %v", typ, id, data, err)
	}
	if req.Op != FileWrite || req.Handle != 3 || req.Offset != 1<<40 {
		t.Fatalf("request header = %+v", req)
	}
	var reply HostFSReply
	typ, id, data, err = ReadHostFS(&buf, &reply)
	if err != nil || typ != FrameHostFSReply || id != 8 || len(data) != 0 || reply.Errno != "ENOENT" {
		t.Fatalf("reply = %#x %d %q %+v %v", typ, id, data, reply, err)
	}
}

func TestHostFSHeaderPastTheFrame(t *testing.T) {
	var payload [12]byte
	binary.BigEndian.PutUint32(payload[4:], 100)
	var buf bytes.Buffer
	WriteFrame(&buf, FrameHostFSReply, payload[:])
	var reply HostFSReply
	if _, _, _, err := ReadHostFS(&buf, &reply); err == nil {
		t.Fatal("a header longer than its frame was accepted")
	}
}

func TestHostFSFrameFitsTheLargestData(t *testing.T) {
	var buf bytes.Buffer
	req := HostFSRequest{Op: FileWrite, Path: string(bytes.Repeat([]byte("a/"), 2000)), Handle: 1 << 62, Offset: 1 << 62}
	if err := WriteHostFS(&buf, FrameHostFSRequest, 1, req, make([]byte, MaxHostFSData)); err != nil {
		t.Fatalf("a full write doesn't fit a frame: %v", err)
	}
}
