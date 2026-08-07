package sessions

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/xormania/wfc/internal/store"
)

func newTestStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state", "control.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewWithClock(db, now)
}

// A session nobody has heard from goes stale, never dead: wfc did not observe
// it terminate, and saying more than it knows is how you end up with two live
// workers.
func TestSessionGoesStaleButNotDead(t *testing.T) {
	clock := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t, func() time.Time { return clock })

	got, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "auditor", Cycle: 3,
		Client: "grok", SessionID: "sess-1", TTL: 10 * time.Minute,
	}, false)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if got.Liveness != LiveRunning {
		t.Errorf("liveness = %q, want %q", got.Liveness, LiveRunning)
	}

	clock = clock.Add(11 * time.Minute)
	after, found, err := s.Get(t.Context(), "p1", "auditor", 3)
	if err != nil || !found {
		t.Fatalf("get: %v, found=%v", err, found)
	}
	if after.Liveness != LiveStale {
		t.Errorf("liveness = %q, want %q", after.Liveness, LiveStale)
	}
	if after.Status != StatusRunning {
		t.Errorf("status = %q; stale is a judgement about silence, not an observed exit", after.Status)
	}

	// A stale session no longer blocks a replacement.
	if _, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "auditor", Cycle: 3, Client: "grok", SessionID: "sess-2",
	}, false); err != nil {
		t.Errorf("recording over a stale session: %v", err)
	}
}

func TestHeartbeatKeepsTheOriginalStartTime(t *testing.T) {
	clock := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t, func() time.Time { return clock })

	first, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "auditor", Client: "grok", SessionID: "sess-1",
	}, false)
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	clock = clock.Add(5 * time.Minute)
	again, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "auditor", Client: "grok", SessionID: "sess-1",
	}, false)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !again.Started.Equal(first.Started) {
		t.Errorf("started moved from %s to %s", first.Started, again.Started)
	}
	if !again.LastSeen.After(first.LastSeen) {
		t.Error("last_seen did not advance")
	}
}

func TestRecordRejectsBadStatus(t *testing.T) {
	s := newTestStore(t, time.Now)
	if _, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "r", Status: "finished",
	}, false); err == nil {
		t.Error("accepted a status that is neither running nor terminal")
	}
}

func TestLiveErrorNamesTheIncumbent(t *testing.T) {
	s := newTestStore(t, time.Now)
	if _, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "auditor", Client: "grok", SessionID: "sess-1",
	}, false); err != nil {
		t.Fatalf("record: %v", err)
	}

	_, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "auditor", Client: "grok", SessionID: "sess-2",
	}, false)
	var live *LiveError
	if !errors.As(err, &live) {
		t.Fatalf("error = %v, want a LiveError", err)
	}
	if live.Existing.SessionID != "sess-1" {
		t.Errorf("incumbent = %q, want sess-1", live.Existing.SessionID)
	}
}
