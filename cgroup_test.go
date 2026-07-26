package main

// FakeResolver implements CgroupResolver for testing.
type FakeResolver struct {
	ID uint64
}

func NewFakeResolver(id uint64) *FakeResolver {
	return &FakeResolver{ID: id}
}

func (f *FakeResolver) Resolve(_ string) ([]CgroupInfo, error) {
	return []CgroupInfo{{
		CgroupID:   f.ID,
		CgroupPath: "/sys/fs/cgroup/fake",
	}}, nil
}
