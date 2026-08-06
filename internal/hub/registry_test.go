package hub

import "testing"

func TestRegistryLastRemovalClosesAdmission(t *testing.T) {
	registry := newRegistry()
	first := &Client{}
	if !registry.add(first) {
		t.Fatal("initial client was rejected")
	}

	removed, remaining := registry.remove(first)
	if !removed || remaining != 0 {
		t.Fatalf("remove = (%v, %d), want (true, 0)", removed, remaining)
	}
	if registry.add(&Client{}) {
		t.Fatal("client admitted after last removal initiated teardown")
	}
}

func TestRegistryConcurrentReplacementKeepsAdmissionOpen(t *testing.T) {
	registry := newRegistry()
	first := &Client{}
	second := &Client{}
	if !registry.add(first) || !registry.add(second) {
		t.Fatal("clients were rejected before teardown")
	}

	removed, remaining := registry.remove(first)
	if !removed || remaining != 1 {
		t.Fatalf("remove = (%v, %d), want (true, 1)", removed, remaining)
	}
	if !registry.add(&Client{}) {
		t.Fatal("client rejected while another client kept the hub alive")
	}
}
