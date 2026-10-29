package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Android folders mounted in the guest (docs/architecture.md, Integrations). The
// app grants folders and announces them with a host-folders request; the
// agent mounts each with FUSE and asks the app for its files over hostfs
// streams, which only the app can dial.

// TypeHostFolders (app → agent, Request.Folders) replaces the set of mounted
// folders: new names are mounted, missing ones unmounted. Answered by its own
// type, or an Error naming the folders that couldn't be mounted (the rest are).
const TypeHostFolders = "host-folders"

// HostFolder is one folder the app shares.
type HostFolder struct {
	// Name is its directory under the mount point: a single path element.
	Name     string `json:"name"`
	Writable bool   `json:"writable,omitempty"`
}

// StreamHostFS carries one folder's file operations, Hello.Name naming the
// folder. The roles flip after the Hellos: the agent sends HostFSRequest
// frames and the app answers each with a HostFSReply frame carrying its ID,
// in any order. The app keeps a few open per folder; while it has none, the
// mount's operations fail with EIO.
const StreamHostFS = "hostfs"

// Frames on a hostfs stream ([WriteFrame]'s format), both
// [id u32][header length u32][JSON header][data].
const (
	// FrameHostFSRequest is the agent's: a HostFSRequest, and a write's bytes.
	FrameHostFSRequest byte = 0x30
	// FrameHostFSReply is the app's: a HostFSReply, and a read's bytes.
	FrameHostFSReply byte = 0x31
)

// Operations on a hostfs stream, in HostFSRequest.Op. Paths are relative to
// the folder, slash-separated; "" is the folder itself.
//
//   - FileStat: Path → File.
//   - FileList: Path, a directory → Files.
//   - FileCreate: an empty file (or Dir a directory) named Name in the
//     directory Path → File. The app may pick another name when Name is
//     taken (Android's providers do); the agent checks.
//   - FileDelete: Path, recursively for a directory.
//   - FileRename: Path to the path To, which doesn't exist; Errno EXDEV when
//     the provider can't move it to another directory → File.
//   - FileOpen: Path in Mode, an Android open mode ("r", "rw", "rwt") →
//     Handle.
//   - FileRead: Size bytes of Handle at Offset → the bytes, fewer only at
//     the end of the file.
//   - FileWrite: the data to Handle at Offset → Size, the count written.
//   - FileTruncate: Handle (or Path, when Handle is 0) to Size bytes.
//   - FileSync: flushes Handle's writes to storage.
//   - FileClose: releases Handle.
const (
	FileTruncate = "truncate"
	FileSync     = "sync"
	FileClose    = "close"
)

// HostFSRequest is a hostfs operation's header.
type HostFSRequest struct {
	Op     string `json:"op"`
	Path   string `json:"path,omitempty"`
	Name   string `json:"name,omitempty"`
	To     string `json:"to,omitempty"`
	Dir    bool   `json:"dir,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Handle int64  `json:"handle,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// HostFSReply answers a HostFSRequest; Error is set when it failed.
type HostFSReply struct {
	Error string `json:"error,omitempty"`
	// Errno names the failure as Linux does ("ENOENT", "EEXIST", "ENOTDIR",
	// "EISDIR", "ENOTEMPTY", "EACCES", "EROFS", "EXDEV", "ENOSPC", …); empty
	// or unknown is EIO.
	Errno  string     `json:"errno,omitempty"`
	File   *FileInfo  `json:"file,omitempty"`
	Files  []FileInfo `json:"files,omitempty"`
	Handle int64      `json:"handle,omitempty"`
	Size   int64      `json:"size,omitempty"`
}

// MaxHostFSData bounds a frame's data: a FUSE read or write is at most this.
const MaxHostFSData = 512 << 10

const hostFSPrefix = 8

// WriteHostFS writes one hostfs frame in a single Write call.
func WriteHostFS(w io.Writer, typ byte, id uint32, header any, data []byte) error {
	h, err := json.Marshal(header)
	if err != nil {
		return err
	}
	b := make([]byte, hostFSPrefix+len(h)+len(data))
	binary.BigEndian.PutUint32(b[0:], id)
	binary.BigEndian.PutUint32(b[4:], uint32(len(h)))
	copy(b[hostFSPrefix:], h)
	copy(b[hostFSPrefix+len(h):], data)
	return WriteFrame(w, typ, b)
}

// ReadHostFS reads one hostfs frame, decoding its header into header.
func ReadHostFS(r io.Reader, header any) (typ byte, id uint32, data []byte, err error) {
	typ, b, err := ReadFrame(r)
	if err != nil {
		return 0, 0, nil, err
	}
	if len(b) < hostFSPrefix {
		return 0, 0, nil, fmt.Errorf("hostfs frame of %d bytes", len(b))
	}
	id = binary.BigEndian.Uint32(b)
	n := binary.BigEndian.Uint32(b[4:])
	if uint64(n) > uint64(len(b)-hostFSPrefix) {
		return 0, 0, nil, fmt.Errorf("hostfs header of %d bytes in a %d-byte frame", n, len(b))
	}
	if err := json.Unmarshal(b[hostFSPrefix:hostFSPrefix+n], header); err != nil {
		return 0, 0, nil, err
	}
	return typ, id, b[hostFSPrefix+n:], nil
}
