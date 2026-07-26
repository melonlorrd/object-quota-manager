// SPDX-License-Identifier: GPL-2.0-only

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>


char __license[] SEC("license") = "GPL";

struct cpu_lease {
	__u64 count;
	__u64 lease_budget;
	__u64 last_used_ns;
	__u64 top_cgid;
	__u64 top_count;
};

enum breach_reason {
	REASON_FD   = 0,
	REASON_PROC = 1,
};

struct breach_event {
	__u64 ns_key;
	__u64 cgid;
	__u32 pid;
	__u32 reason;
	__u64 timestamp;
	__u64 top_cgid;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);
	__type(value, __u64);
} fd_cgroup SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);
	__type(value, __u64);
} fd_cgroup_counter SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, __u64);
	__type(value, __u64);
} fd_ns_limit SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, __u64);
	__type(value, struct cpu_lease);
} fd_ns_counter SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);
	__type(value, __u64);
} proc_cgroup SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);
	__type(value, __u64);
} proc_cgroup_counter SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, __u64);
	__type(value, __u64);
} proc_ns_limit SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, __u64);
	__type(value, struct cpu_lease);
} proc_ns_counter SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 64 * 1024);
	__type(value, struct breach_event);
} events SEC(".maps");

static __always_inline void emit_breach_event(__u64 ns_key, __u64 cgid, __u64 top_cgid, __u32 reason)
{
	struct breach_event *evt = bpf_ringbuf_reserve(&events, sizeof(*evt), 0);
	if (evt) {
		evt->ns_key = ns_key;
		evt->cgid = cgid;
		evt->pid = (__u32)(bpf_get_current_pid_tgid() >> 32);
		evt->reason = reason;
		evt->timestamp = bpf_ktime_get_ns();
		evt->top_cgid = top_cgid;
		bpf_ringbuf_submit(evt, 0);
	}
}

static __always_inline void record_proc_cgroup_inc(__u64 cgid)
{
	__u64 *cg_count = bpf_map_lookup_elem(&proc_cgroup_counter, &cgid);
	if (!cg_count) {
		__u64 one = 1;
		bpf_map_update_elem(&proc_cgroup_counter, &cgid, &one, BPF_ANY);
	} else {
		__sync_fetch_and_add(cg_count, 1);
	}
}

static __always_inline void record_proc_cgroup_dec(__u64 cgid)
{
	__u64 *cg_count = bpf_map_lookup_elem(&proc_cgroup_counter, &cgid);
	if (cg_count && *cg_count > 0) {
		__sync_fetch_and_sub(cg_count, 1);
	}
}

static __always_inline int over_proc_limit(void)
{
	__u64 cgid = bpf_get_current_cgroup_id();
	__u64 *ns_key = bpf_map_lookup_elem(&proc_cgroup, &cgid);
	if (!ns_key)
		return 0;

	record_proc_cgroup_inc(cgid);

	__u64 *limit = bpf_map_lookup_elem(&proc_ns_limit, ns_key);
	if (!limit || *limit == 0)
		return 0;

	struct cpu_lease *lease = bpf_map_lookup_elem(&proc_ns_counter, ns_key);
	if (!lease)
		return 0;

	__u64 *cg_count = bpf_map_lookup_elem(&proc_cgroup_counter, &cgid);
	if (cg_count && *cg_count > lease->top_count) {
		lease->top_cgid = cgid;
		lease->top_count = *cg_count;
	}

	lease->last_used_ns = bpf_ktime_get_ns();

	if (lease->count >= *limit) {
		emit_breach_event(*ns_key, cgid, lease->top_cgid, REASON_PROC);
		if (cgid == lease->top_cgid)
			return 1;
		return 2;
	}

	if (lease->count >= lease->lease_budget) {
		__u64 step = *limit / 10;
		if (step < 10)
			step = 10;
		if (step > 100)
			step = 100;
		if (lease->lease_budget + step <= *limit) {
			lease->lease_budget += step;
		} else {
			lease->lease_budget = *limit;
		}
		if (lease->count >= lease->lease_budget) {
			emit_breach_event(*ns_key, cgid, lease->top_cgid, REASON_PROC);
			if (cgid == lease->top_cgid)
				return 1;
			return 2;
		}
	}

	__sync_fetch_and_add(&lease->count, 1);
	return 0;
}

SEC("fentry/kernel_clone")
int BPF_PROG(track_clone, void *args)
{
	int ret = over_proc_limit();
	if (ret == 1)
		bpf_send_signal(9);
	return 0;
}

SEC("fentry/do_exit")
int BPF_PROG(track_exit, long code)
{
	__u64 cgid = bpf_get_current_cgroup_id();
	record_proc_cgroup_dec(cgid);

	__u64 *ns_key = bpf_map_lookup_elem(&proc_cgroup, &cgid);
	if (!ns_key)
		return 0;

	struct cpu_lease *lease = bpf_map_lookup_elem(&proc_ns_counter, ns_key);
	if (lease && lease->count > 0) {
		__sync_fetch_and_sub(&lease->count, 1);
		lease->last_used_ns = bpf_ktime_get_ns();
	}
	return 0;
}

static __always_inline void record_cgroup_inc(__u64 cgid)
{
	__u64 *cg_count = bpf_map_lookup_elem(&fd_cgroup_counter, &cgid);
	if (!cg_count) {
		__u64 one = 1;
		bpf_map_update_elem(&fd_cgroup_counter, &cgid, &one, BPF_ANY);
	} else {
		__sync_fetch_and_add(cg_count, 1);
	}
}

static __always_inline void record_cgroup_dec(__u64 cgid)
{
	__u64 *cg_count = bpf_map_lookup_elem(&fd_cgroup_counter, &cgid);
	if (cg_count && *cg_count > 0) {
		__sync_fetch_and_sub(cg_count, 1);
	}
}

static __always_inline int over_limit(void)
{
	__u64 cgid = bpf_get_current_cgroup_id();
	__u64 *ns_key = bpf_map_lookup_elem(&fd_cgroup, &cgid);
	if (!ns_key)
		return 0;

	record_cgroup_inc(cgid);

	__u64 *limit = bpf_map_lookup_elem(&fd_ns_limit, ns_key);
	if (!limit || *limit == 0)
		return 0;

	struct cpu_lease *lease = bpf_map_lookup_elem(&fd_ns_counter, ns_key);
	if (!lease)
		return 0;

	__u64 *cg_count = bpf_map_lookup_elem(&fd_cgroup_counter, &cgid);
	if (cg_count && *cg_count > lease->top_count) {
		lease->top_cgid = cgid;
		lease->top_count = *cg_count;
	}
#ifdef BPF_DEBUG
	bpf_printk("fd_top: cgid=%llu top=%llu topc=%llu", cgid, lease->top_cgid, lease->top_count);
#endif

	lease->last_used_ns = bpf_ktime_get_ns();

	if (lease->count >= *limit) {
		emit_breach_event(*ns_key, cgid, lease->top_cgid, REASON_FD);
		if (cgid == lease->top_cgid)
			return 1;
		return 2;
	}

	if (lease->count >= lease->lease_budget) {
		__u64 step = *limit / 10;
		if (step < 10)
			step = 10;
		if (step > 100)
			step = 100;
		if (lease->lease_budget + step <= *limit) {
			lease->lease_budget += step;
		} else {
			lease->lease_budget = *limit;
		}
		if (lease->count >= lease->lease_budget) {
			emit_breach_event(*ns_key, cgid, lease->top_cgid, REASON_FD);
			if (cgid == lease->top_cgid)
				return 1;
			return 2;
		}
	}

	__sync_fetch_and_add(&lease->count, 1);
	return 0;
}

SEC("fentry/fd_install")
int BPF_PROG(track_install_fd, unsigned int fd, struct file *file)
{
	int ret = over_limit();
	if (ret == 1)
		bpf_send_signal(9);
	return 0;
}

SEC("fentry/filp_close")
int BPF_PROG(track_close_fd, struct file *file, void *id)
{
	if (!file)
		return 0;

	long refs = file->f_count.counter;
	if (refs > 1)
		return 0;

	__u64 cgid = bpf_get_current_cgroup_id();
	record_cgroup_dec(cgid);

	__u64 *ns_key = bpf_map_lookup_elem(&fd_cgroup, &cgid);
	if (!ns_key)
		return 0;

	struct cpu_lease *lease = bpf_map_lookup_elem(&fd_ns_counter, ns_key);
	if (!lease || lease->count == 0)
		return 0;

	__sync_fetch_and_sub(&lease->count, 1);
	lease->last_used_ns = bpf_ktime_get_ns();
	return 0;
}

