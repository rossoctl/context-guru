package accrec

import (
	"fmt"
	"testing"

	"github.com/rossoctl/accountant"
)

func TestPingStoreTakeDeletesAndOverwriteKeepsLatest(t *testing.T) {
	s := NewPingStore()
	s.Put("k", accountant.PingFacts{PingWindow: "a"})
	s.Put("k", accountant.PingFacts{PingWindow: "b"})
	f, ok := s.Take("k")
	if !ok || f.PingWindow != "b" {
		t.Fatalf("%+v %v", f, ok)
	}
	if _, ok := s.Take("k"); ok {
		t.Fatal("take must delete")
	}
	var nilStore *PingStore
	nilStore.Put("k", accountant.PingFacts{})
	if _, ok := nilStore.Take("k"); ok {
		t.Fatal("nil store")
	}
	if nilStore.Len() != 0 || nilStore.Priced() != 0 {
		t.Fatal("nil store must read empty")
	}
}

func TestPingStoreIsBounded(t *testing.T) {
	s := NewPingStore()
	for i := 0; i < PingStoreMax+500; i++ {
		s.Put(fmt.Sprint("k", i), accountant.PingFacts{PingWindow: "w"})
	}
	if s.Len() > PingStoreMax {
		t.Fatalf("len %d > %d", s.Len(), PingStoreMax)
	}
	if _, ok := s.Take(fmt.Sprint("k", PingStoreMax+499)); !ok {
		t.Fatal("the newest entry must survive")
	}
}

func TestPingStoreDropKeepsANewerWindow(t *testing.T) {
	s := NewPingStore()
	s.Put("k", accountant.PingFacts{PingWindow: "w2"})
	s.Drop("k", "w1") // the request read w1; w2 is a newer ping
	if _, ok := s.Peek("k"); !ok {
		t.Fatal("a newer ping's facts must stay")
	}
	s.Drop("k", "")
	if _, ok := s.Peek("k"); ok {
		t.Fatal("an empty window drops whatever is there")
	}
}
