package ebpf

import "github.com/cilium/ebpf"

type BpfObjects = bpfObjects
type BpfCpuLease = bpfCpuLease
type BpfBreachEvent = bpfBreachEvent

func LoadBpfObjects(obj any, opts *ebpf.CollectionOptions) error {
	return loadBpfObjects(obj, opts)
}
