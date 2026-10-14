package capacity

import "testing"

// TestBreachCounterCountsRefusals proves the guard counts each admission refusal
// by reason, and that a successful admission counts nothing.
func TestBreachCounterCountsRefusals(t *testing.T) {
	dir := t.TempDir()
	free, ok := BackingAvailableBytes(dir)
	if !ok {
		t.Skip("backing free space unmeasurable")
	}

	// Positive headroom but too little for a large transaction: two refusals.
	g := New(Policy{ReserveBytes: free / 2})
	if err := g.CheckBackingReserve(dir, free); err == nil {
		t.Fatal("expected a backing-reserve refusal")
	}
	if got := g.BreachCounts()[BreachBackingReserve]; got != 1 {
		t.Fatalf("backing_reserve breaches = %d, want 1", got)
	}
	if err := g.CheckBackingReserve(dir, free); err == nil {
		t.Fatal("expected a second backing-reserve refusal")
	}
	if got := g.BreachCounts()[BreachBackingReserve]; got != 2 {
		t.Fatalf("backing_reserve breaches = %d, want 2", got)
	}

	// A small transaction that clears the headroom counts nothing.
	if err := g.CheckBackingReserve(dir, 1); err != nil {
		t.Fatalf("small transaction refused: %v", err)
	}
	if got := g.BreachCounts()[BreachBackingReserve]; got != 2 {
		t.Fatalf("successful admission changed the counter: %d", got)
	}
}

// TestBreachCounterAuthoritativeAndQuota proves the explicit bounds are counted
// under their own reasons.
func TestBreachCounterAuthoritativeAndQuota(t *testing.T) {
	limit := uint64(1000)
	ga := New(Policy{AuthoritativeCapacityBytes: &limit})
	if err := ga.CheckAuthoritative(0, 2000); err == nil {
		t.Fatal("expected an authoritative refusal")
	}
	if got := ga.BreachCounts()[BreachAuthoritative]; got != 1 {
		t.Fatalf("authoritative breaches = %d, want 1", got)
	}

	quota := uint64(500)
	gq := New(Policy{HardQuotaBytes: &quota})
	if err := gq.CheckAuthoritative(0, 600); err == nil {
		t.Fatal("expected a hard-quota refusal")
	}
	if got := gq.BreachCounts()[BreachHardQuota]; got != 1 {
		t.Fatalf("hard_quota breaches = %d, want 1", got)
	}
}

// TestBreachCountsIsACopy proves callers cannot mutate the governor through the
// returned map.
func TestBreachCountsIsACopy(t *testing.T) {
	limit := uint64(10)
	g := New(Policy{AuthoritativeCapacityBytes: &limit})
	_ = g.CheckAuthoritative(0, 100)
	counts := g.BreachCounts()
	counts[BreachAuthoritative] = 999
	if got := g.BreachCounts()[BreachAuthoritative]; got != 1 {
		t.Fatalf("governor counter mutated through a copy: %d", got)
	}
}
