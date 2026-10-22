// SPDX-License-Identifier: GPL-2.0
//
// fastpath.c — LimitPlane's kernel fast path.
//
// An XDP program runs inside the network driver, on the raw frame, BEFORE the
// kernel allocates an sk_buff for it. Dropping a packet here costs a few tens
// of nanoseconds; dropping the same packet in the Go gateway costs a TCP
// handshake, a goroutine, an HTTP parse and a 429 response. So this file does
// only the decisions that are cheap and obvious, and leaves everything that
// needs meaning (tenants, cost classes, monthly plans) to the Go layer:
//
//   1. BANNED?  the source IP is on the ban list → drop.
//               The list is written by the userspace agent, which copies the
//               gateway's autopilot cooldowns in here.
//   2. FLOOD?   the source IP has spent its per-IP packet budget → drop.
//               A plain token bucket, refilled from bpf_ktime_get_ns().
//   3. Otherwise XDP_PASS, and the packet continues to the kernel stack and,
//      eventually, to the Go gateway as usual.
//
// Everything is keyed by a 16-byte address. IPv4 is stored IPv4-mapped
// (::ffff:a.b.c.d), so one map serves both families and userspace converts
// with netip.Addr.As16().

#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

char __license[] SEC("license") = "GPL";

#define NSEC_PER_SEC 1000000000ULL

// Counter slots in the per-CPU `counters` array. Userspace sums every CPU.
enum {
	CNT_PASSED = 0,
	CNT_DROPPED_BANNED = 1,
	CNT_DROPPED_RATE = 2,
	CNT_MAX,
};

struct ip_key {
	__u8 addr[16];
};

// A ban. until_ns is CLOCK_MONOTONIC (the clock bpf_ktime_get_ns reads), so a
// ban expires in the kernel on its own even if the agent that wrote it dies.
// 0 means "until removed".
struct ban {
	__u64 until_ns;
};

// One per-IP jar. tokens is scaled by NSEC_PER_SEC so that refill can be done
// in integer maths: every elapsed nanosecond adds `rate_pps` scaled units, and
// a packet costs exactly NSEC_PER_SEC of them. No floating point exists in BPF.
struct bucket {
	__u64 tokens;
	__u64 last_ns;
};

// Written once by userspace. rate_pps == 0 disables the flood check.
struct config {
	__u64 rate_pps;
	__u64 burst;
};

// LRU, not a plain hash: the key is attacker-controlled (any source address),
// so a plain hash fills up and then refuses new entries. An LRU evicts the
// least recently seen address instead, so a spoofed-source flood can only
// churn the table, never wedge it.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, struct ip_key);
	__type(value, struct ban);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} banned SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 262144);
	__type(key, struct ip_key);
	__type(value, struct bucket);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} buckets SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct config);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} config SEC(".maps");

// Per-CPU so that counting never needs an atomic: each CPU bumps its own slot.
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, CNT_MAX);
	__type(key, __u32);
	__type(value, __u64);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} counters SEC(".maps");

// count bumps one counter slot on the current CPU.
static __always_inline void count(__u32 slot)
{
	__u64 *v = bpf_map_lookup_elem(&counters, &slot);

	if (v)
		*v += 1;
}

// source_key fills `key` with the packet's source address, or returns -1 for
// anything that is not IPv4/IPv6 (ARP, LLDP, ...), which is always passed.
//
// Every pointer is checked against data_end before it is read. The verifier
// rejects the program at load time if a single read could run past the frame.
static __always_inline int source_key(void *data, void *data_end, struct ip_key *key)
{
	struct ethhdr *eth = data;

	if ((void *)(eth + 1) > data_end)
		return -1;

	if (eth->h_proto == bpf_htons(ETH_P_IP)) {
		struct iphdr *ip = (void *)(eth + 1);

		if ((void *)(ip + 1) > data_end)
			return -1;
		__builtin_memset(key, 0, sizeof(*key));
		key->addr[10] = 0xff;
		key->addr[11] = 0xff;
		__builtin_memcpy(&key->addr[12], &ip->saddr, 4);
		return 0;
	}

	if (eth->h_proto == bpf_htons(ETH_P_IPV6)) {
		struct ipv6hdr *ip6 = (void *)(eth + 1);

		if ((void *)(ip6 + 1) > data_end)
			return -1;
		__builtin_memcpy(key->addr, &ip6->saddr, 16);
		return 0;
	}

	return -1;
}

// is_banned answers check 1. An expired ban is deleted on the spot, so the
// table cleans itself even when nobody in userspace is reconciling it.
static __always_inline int is_banned(struct ip_key *key, __u64 now)
{
	struct ban *b = bpf_map_lookup_elem(&banned, key);

	if (!b)
		return 0;
	if (b->until_ns && now >= b->until_ns) {
		bpf_map_delete_elem(&banned, key);
		return 0;
	}
	return 1;
}

// over_budget answers check 2: refill the jar for the time since we last saw
// this address, then try to pay one packet.
//
// The race, honestly: the bucket is read, modified and written through a
// pointer with no lock, and two CPUs can process packets from the same source
// at the same time. One of their updates can then be lost, so under a
// multi-queue flood the limit is approximate — it can let a few extra packets
// through, or refuse a few early. That trade is deliberate: a bpf_spin_lock
// per packet would serialise exactly the traffic this is meant to absorb.
// In practice the NIC's RSS hashes one flow to one RX queue, so one TCP
// connection is always handled by one CPU; only a source spreading many flows
// over many queues sees the approximation. The exact, tenant-aware limit is
// still enforced by the Go layer behind this one.
static __always_inline int over_budget(struct ip_key *key, __u64 now)
{
	__u32 zero = 0;
	struct config *cfg = bpf_map_lookup_elem(&config, &zero);

	if (!cfg || cfg->rate_pps == 0 || cfg->burst == 0)
		return 0;

	__u64 cap = cfg->burst * NSEC_PER_SEC;
	struct bucket *b = bpf_map_lookup_elem(&buckets, key);

	if (!b) {
		// First packet from this address: a full jar, minus this packet.
		struct bucket fresh = { .tokens = cap - NSEC_PER_SEC, .last_ns = now };

		bpf_map_update_elem(&buckets, key, &fresh, BPF_ANY);
		return 0;
	}

	__u64 elapsed = now > b->last_ns ? now - b->last_ns : 0;
	__u64 tokens = b->tokens;

	// Long idle: the jar is full no matter what, and skipping the multiply
	// keeps elapsed * rate_pps far away from overflowing 64 bits.
	if (elapsed >= NSEC_PER_SEC * 60)
		tokens = cap;
	else
		tokens += elapsed * cfg->rate_pps;
	if (tokens > cap)
		tokens = cap;

	b->last_ns = now;
	if (tokens < NSEC_PER_SEC) {
		b->tokens = tokens;
		return 1;
	}
	b->tokens = tokens - NSEC_PER_SEC;
	return 0;
}

SEC("xdp")
int limitplane_fastpath(struct xdp_md *ctx)
{
	void *data = (void *)(long)ctx->data;
	void *data_end = (void *)(long)ctx->data_end;
	struct ip_key key;

	if (source_key(data, data_end, &key) < 0)
		return XDP_PASS;

	__u64 now = bpf_ktime_get_ns();

	if (is_banned(&key, now)) {
		count(CNT_DROPPED_BANNED);
		return XDP_DROP;
	}
	if (over_budget(&key, now)) {
		count(CNT_DROPPED_RATE);
		return XDP_DROP;
	}
	count(CNT_PASSED);
	return XDP_PASS;
}
