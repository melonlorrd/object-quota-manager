package main

import (
	"hash/fnv"
)

type MapKind int

const (
	MapFDCgroup MapKind = iota
	MapFDNsLimit
	MapFDNsCounter
	MapProcCgroup
	MapProcNsLimit
	MapProcNsCounter
)


func NamespaceKey(ns string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(ns))
	sum := h.Sum64()
	return sum
}

type StubManager struct{}

func NewStubManager() *StubManager { return &StubManager{} }

func (s *StubManager) UpdateElement(kind MapKind, key, value any) error { return nil }
func (s *StubManager) DeleteElement(kind MapKind, key any) error { return nil }
