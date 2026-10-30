//go:build ignore

// Wakes userspace when a TCP socket enters or leaves LISTEN. The tracepoint
// fires before the port is hashed and in every netns, so userspace treats an
// event as a hint and rescans /proc/net/tcp*.

#include "common.h"

char __license[] SEC("license") = "GPL";

#define TCP_CLOSE 7
#define TCP_LISTEN 10

// The raw tracepoint's TP_PROTO: (const struct sock *sk, int oldstate, int newstate).
struct bpf_raw_tracepoint_args {
	__u64 args[0];
};

// CO-RE: field offsets are relocated against the running kernel's BTF at load.
struct sock_common {
	__u16 skc_num;
} __attribute__((preserve_access_index));

struct sock {
	struct sock_common __sk_common;
} __attribute__((preserve_access_index));

struct event {
	// Host order; 0 for a socket that listen()s without bind().
	__u16 port;
	__u16 pad;
	__s32 oldstate;
	__s32 newstate;
};
// Keeps struct event in the BTF for bpf2go's -type.
const struct event *unused_event __attribute__((unused));

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 12);
} events SEC(".maps");

SEC("raw_tracepoint/inet_sock_set_state")
int inet_sock_set_state(struct bpf_raw_tracepoint_args *ctx) {
	struct event ev = {
		.oldstate = (int)ctx->args[1],
		.newstate = (int)ctx->args[2],
	};
	// LISTEN to SYN_RECV is a listener's child socket, once per connection.
	if (ev.newstate != TCP_LISTEN && !(ev.oldstate == TCP_LISTEN && ev.newstate == TCP_CLOSE))
		return 0;
	struct sock *sk = (struct sock *)ctx->args[0];
	bpf_probe_read_kernel(&ev.port, sizeof(ev.port), &sk->__sk_common.skc_num);
	bpf_ringbuf_output(&events, &ev, sizeof(ev), 0);
	return 0;
}
