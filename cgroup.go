package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var (
	errNotCgroupV2 = errors.New("not a cgroup v2 path")
	errNotFound    = errors.New("cgroup not found")
)

type CgroupInfo struct {
	CgroupID   uint64
	CgroupPath string
}

var CgroupFsRoot = "/sys/fs/cgroup"

type CgroupResolver interface {
	Resolve(podUID string) ([]CgroupInfo, error)
}

type CachedPodResolver struct {
	underlying CgroupResolver
	mu         sync.RWMutex
	cache      map[string][]CgroupInfo
}

func NewCachedPodResolver(underlying CgroupResolver) *CachedPodResolver {
	return &CachedPodResolver{
		underlying: underlying,
		cache:      make(map[string][]CgroupInfo),
	}
}

func (c *CachedPodResolver) Resolve(podUID string) ([]CgroupInfo, error) {
	c.mu.RLock()
	if info, ok := c.cache[podUID]; ok {
		c.mu.RUnlock()
		return info, nil
	}
	c.mu.RUnlock()

	info, err := c.underlying.Resolve(podUID)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.cache[podUID] = info
	c.mu.Unlock()

	return info, nil
}

func (c *CachedPodResolver) Invalidate(podUID string) {
	c.mu.Lock()
	delete(c.cache, podUID)
	c.mu.Unlock()
}

type PodResolver struct{}

func NewPodResolver() *PodResolver { return &PodResolver{} }

func (r *PodResolver) Resolve(podUID string) ([]CgroupInfo, error) {
	cgUID := strings.ReplaceAll(podUID, "-", "_")
	var candidates []string
	candidates = append(candidates,
		filepath.Join(CgroupFsRoot, "kubepods.slice", fmt.Sprintf("kubepods-pod%s.slice", cgUID)),
		filepath.Join(CgroupFsRoot, "kubepods.slice", fmt.Sprintf("pod%s", cgUID)),
	)
	for _, qos := range []string{"besteffort", "burstable", "guaranteed"} {
		candidates = append(candidates,
			filepath.Join(CgroupFsRoot, "kubepods.slice", fmt.Sprintf("kubepods-%s.slice", qos), fmt.Sprintf("kubepods-%s-pod%s.slice", qos, cgUID)),
			filepath.Join(CgroupFsRoot, "kubepods.slice", fmt.Sprintf("kubepods-%s.slice", qos), fmt.Sprintf("pod%s", cgUID)),
		)
	}

	for _, podPath := range candidates {
		info, err := readCgroupID(podPath)
		if err != nil {
			continue
		}

		var result []CgroupInfo
		children, _ := os.ReadDir(podPath)
		for _, child := range children {
			if !child.IsDir() {
				continue
			}
			childPath := filepath.Join(podPath, child.Name())
			childInfo, err := readCgroupID(childPath)
			if err == nil {
				result = append(result, *childInfo)
			}
		}
		result = append(result, *info)
		return result, nil
	}
	return nil, fmt.Errorf("%w: pod %s not found in cgroupfs", errNotFound, podUID)
}

func readCgroupID(path string) (*CgroupInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", errNotFound, path)
		}
		return nil, err
	}

	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", errNotCgroupV2, path)
	}
	return &CgroupInfo{
		CgroupID:   readInode(fi),
		CgroupPath: path,
	}, nil
}

func readInode(fi os.FileInfo) uint64 {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return stat.Ino
}

func PodIDFromCgroupPath(path string) (string, bool) {
	base := filepath.Base(path)
	parts := strings.SplitN(base, "-pod", 2)
	if len(parts) != 2 {
		return "", false
	}
	uid := strings.TrimSuffix(parts[1], ".slice")
	return uid, true
}

func KillCgroupProcs(cgroupPath string) error {
	f, err := os.Open(filepath.Join(cgroupPath, "cgroup.procs"))
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		pidStr := strings.TrimSpace(scanner.Text())
		if pidStr == "" {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		syscall.Kill(pid, syscall.SIGKILL)
	}
	return scanner.Err()
}
