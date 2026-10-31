module github.com/nickolaj-jepsen/pocketnix/go

go 1.26

require (
	github.com/cilium/ebpf v0.22.0
	github.com/creack/pty v1.1.24
	github.com/godbus/dbus/v5 v5.2.4
	github.com/hanwen/go-fuse/v2 v2.12.1
	github.com/mdlayher/vsock v1.3.0
	golang.org/x/sys v0.45.0
)

require (
	github.com/mdlayher/socket v0.6.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
)

tool github.com/cilium/ebpf/cmd/bpf2go
