package store

import "testing"

// The memory store must pass the full conformance suite, always (no Docker/DSN
// required — it is the reference implementation).
func TestMemoryConformance(t *testing.T) {
	testStore(t, func(t *testing.T) Store { return NewMemory() })
}
