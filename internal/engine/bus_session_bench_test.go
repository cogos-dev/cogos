package engine

import (
	"testing"
)

// BenchmarkReadEvents_ColdFullHistory measures the one-time cost of a full
// history scan against a bus with a realistic event count (mirrors the
// live bus_sessions bus's ~70K lines from the #561 pprof evidence, scaled
// down for benchmark runtime).
func BenchmarkReadEvents_ColdFullHistory(b *testing.B) {
	root := b.TempDir()
	mgr := NewBusSessionManager(root)
	busID := "bench-bus"
	const n = 20000
	for i := 0; i < n; i++ {
		if _, err := mgr.AppendEvent(busID, "m", "tester", map[string]interface{}{"i": i}); err != nil {
			b.Fatalf("AppendEvent: %v", err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Force a cold cache each iteration to measure the one-time cost.
		mgr.readCacheMetaMu.Lock()
		mgr.readCache = make(map[string]*busReadCache)
		mgr.readCacheMetaMu.Unlock()
		if _, err := mgr.ReadEvents(busID); err != nil {
			b.Fatalf("ReadEvents: %v", err)
		}
	}
}

// BenchmarkReadEventsSince_SteadyStatePoll measures the actual #561 hot
// path: a poller repeatedly calling ReadEventsSince with its cursor
// advanced to the tip after every call, against the SAME warm cache —
// exactly the pattern handleBusEvents now drives. Cost should stay ~flat
// regardless of total bus history size, since each call only scans the
// zero-to-few new events since the last one.
func BenchmarkReadEventsSince_SteadyStatePoll(b *testing.B) {
	root := b.TempDir()
	mgr := NewBusSessionManager(root)
	busID := "bench-poll-bus"
	const n = 20000
	for i := 0; i < n; i++ {
		if _, err := mgr.AppendEvent(busID, "m", "tester", map[string]interface{}{"i": i}); err != nil {
			b.Fatalf("AppendEvent: %v", err)
		}
	}
	// Warm the cache once, as the first real poll would.
	events, err := mgr.ReadEventsSince(busID, 0)
	if err != nil {
		b.Fatalf("warm ReadEventsSince: %v", err)
	}
	cursor := events[len(events)-1].Seq
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := mgr.ReadEventsSince(busID, cursor); err != nil {
			b.Fatalf("ReadEventsSince: %v", err)
		}
	}
}
