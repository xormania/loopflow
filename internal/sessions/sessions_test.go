package sessions

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xormania/loopflow/internal/store"
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

// A session nobody has heard from goes stale, never dead: loopflow did not observe
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
	after, found, err := s.Get(t.Context(), "p1", "auditor", "", 3)
	if err != nil || !found {
		t.Fatalf("get: %v, found=%v", err, found)
	}
	if after.Liveness != LiveStale {
		t.Errorf("liveness = %q, want %q", after.Liveness, LiveStale)
	}
	if after.Status != StatusRunning {
		t.Errorf("status = %q; stale is a judgement about silence, not an observed exit", after.Status)
	}

	// A stale session still blocks a replacement. loopflow never watched the old
	// worker stop, so it cannot license launching another one.
	_, err = s.Record(t.Context(), Session{
		Packet: "p1", Role: "auditor", Cycle: 3, Client: "grok", SessionID: "sess-2",
	}, false)
	var live *LiveError
	if !errors.As(err, &live) {
		t.Fatalf("recording over a stale session = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "-takeover") {
		t.Errorf("the refusal does not say how to proceed: %v", err)
	}

	// Taking it over is allowed, and the displaced record is kept.
	if _, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "auditor", Cycle: 3, Client: "grok", SessionID: "sess-2",
	}, true); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	history, err := s.db.ListWorkerSessionHistory(t.Context(), "p1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 || history[0].SessionID != "sess-1" {
		t.Errorf("displaced session not archived: %+v", history)
	}
}

// Different role-tasks under the same custody role do not collide: gap-review,
// sensitivity-review, and final-audit are all auditor work at cycle 0.
func TestTaskIsPartOfTheKey(t *testing.T) {
	s := newTestStore(t, time.Now)
	for _, task := range []string{"gap-review", "sensitivity-review", "final-audit"} {
		if _, err := s.Record(t.Context(), Session{
			Packet: "p1", Role: "auditor", Task: task, Cycle: 0,
			Client: "grok", SessionID: "sess-" + task,
		}, false); err != nil {
			t.Fatalf("record %s: %v", task, err)
		}
	}
	list, err := s.List(t.Context(), "p1", false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("got %d sessions, want 3 — auditor/0 collapsed distinct role-tasks", len(list))
	}
}

// Two registrations carrying no identifier are not the same worker. Comparing
// empty identities as equal would fold a second launch into the first.
func TestEmptyIdentitiesAreNotTheSameWorker(t *testing.T) {
	s := newTestStore(t, time.Now)
	if _, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "auditor", Client: "grok",
	}, false); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "auditor", Client: "grok",
	}, false); err == nil {
		t.Error("a second anonymous registration was folded into the first")
	}
}

// A caller that learns the provider session id only after launch can add it to
// a registration made under an agent path.
func TestSessionIDCanBeAddedToAnAgentPathRegistration(t *testing.T) {
	s := newTestStore(t, time.Now)
	if _, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "test_author", Client: "codex",
		AgentPath: "/root/g1_plan_test_author",
	}, false); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := s.Record(t.Context(), Session{
		Packet: "p1", Role: "test_author", Client: "codex",
		AgentPath: "/root/g1_plan_test_author", SessionID: "learned-later",
	}, false)
	if err != nil {
		t.Fatalf("adding the session id: %v", err)
	}
	if got.SessionID != "learned-later" {
		t.Errorf("session id = %q", got.SessionID)
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
