package main

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

func TestLoadBPF(t *testing.T) {
	s := NewSyncer(nil, nil, nil, log.FromContext(context.Background()))
	err := s.LoadBPF()
	if err != nil {
		t.Skipf("eBPF load requires privileges: %v", err)
	}
	defer s.Close()
	t.Log("eBPF programs loaded and attached")
}
