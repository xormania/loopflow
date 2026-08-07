// Package sessions records which provider session is working which role-task,
// and when it was last seen.
//
// Nothing correlates the identities the clients hand back — a Codex agent path
// and rollout session id, a Grok sessionId, a transient numeric process handle
// — so "is the worker I launched still alive?" is currently answered by
// remembering, and a forgotten answer costs a duplicate launch. Recording the
// identity against the role-task makes that a lookup.
//
// wfc does not launch workers and does not observe them exit (decisions
// elsewhere own both). It only knows what it was told and when. So a session
// past its TTL is reported stale, never dead: claiming a worker is gone when
// nobody watched it terminate is exactly how a loop ends up with two live ones.
package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xormania/wfc/internal/store"
)

// DefaultTTL is how long a session is assumed live without being seen again.
// Observed Grok audits ran 13-24 minutes and test-author corrections 3-10, so
// half an hour is comfortably longer than a healthy turn.
const DefaultTTL = 30 * time.Minute

// timeFormat is fixed-width so string comparison is chronological.
const timeFormat = "2006-01-02T15:04:05.000000Z"

// Status values a caller sets.
const (
	StatusRunning  = "running"
	StatusTerminal = "terminal"
)

// Liveness values wfc derives.
const (
	LiveRunning  = "running"
	LiveStale    = "stale"
	LiveTerminal = "terminal"
)

// ErrLive reports that a different session is already registered for a
// role-task, live or stale. Both refuse; the difference is what the caller
// should do about it.
var ErrLive = errors.New("a session is already registered for this role-task")

// LiveError carries the session that is in the way, so the caller can resume it
// instead of launching a second one.
type LiveError struct{ Existing Session }

func (e *LiveError) Error() string {
	id := e.Existing.SessionID
	if id == "" {
		id = e.Existing.AgentPath
	}
	if id == "" {
		id = "(no session id recorded)"
	}
	advice := "resume it rather than launching another"
	if e.Existing.Liveness == LiveStale {
		// Stale means wfc has not heard from it, not that it stopped. wfc never
		// watches a worker exit, so it cannot license a replacement; only the
		// caller can, having checked.
		advice = "it has gone quiet, which is not the same as having stopped — " +
			"verify the process, then re-run with -takeover if it is really gone"
	}
	return fmt.Sprintf("%s %s cycle %d is held by a %s %s session %s, last seen %s; %s",
		e.Existing.Packet, e.Existing.RoleTask(), e.Existing.Cycle,
		e.Existing.Liveness, e.Existing.Client, id,
		e.Existing.LastSeen.Format(timeFormat), advice)
}

// RoleTask renders the key's role and task together.
func (s Session) RoleTask() string {
	if s.Task == "" {
		return s.Role
	}
	return s.Role + "/" + s.Task
}

// sameWorker reports whether an incoming registration describes the worker an
// existing one already describes.
//
// Registrations with no identifier at all are never the same worker. Comparing
// two empty identities as equal would silently fold a second launch into the
// first — the exact duplicate this registry exists to catch. Matching on either
// identifier lets a caller that learns a provider session id only after launch
// add it to a registration made under an agent path.
func sameWorker(existing, in Session) bool {
	if existing.SessionID == "" && existing.AgentPath == "" {
		return false
	}
	if in.SessionID == "" && in.AgentPath == "" {
		return false
	}
	if existing.AgentPath != "" && existing.AgentPath == in.AgentPath {
		return true
	}
	return existing.SessionID != "" && existing.SessionID == in.SessionID
}

func (e *LiveError) Unwrap() []error { return []error{ErrLive} }

// Session is one provider session working one role-task.
type Session struct {
	Packet    string        `json:"packet"`
	Role      string        `json:"role"`
	Task      string        `json:"task,omitempty"`
	Cycle     int64         `json:"cycle"`
	Client    string        `json:"client"`
	SessionID string        `json:"session_id,omitempty"`
	AgentPath string        `json:"agent_path,omitempty"`
	Parent    string        `json:"parent,omitempty"`
	PID       int64         `json:"pid,omitempty"`
	Status    string        `json:"status"`
	Reason    string        `json:"reason,omitempty"`
	Note      string        `json:"note,omitempty"`
	Started   time.Time     `json:"started"`
	LastSeen  time.Time     `json:"last_seen"`
	TTL       time.Duration `json:"-"`

	// TTLSeconds is what callers read: a duration in JSON should be a number
	// with a stated unit, not Go's nanosecond count.
	TTLSeconds int64 `json:"ttl_seconds"`

	// Liveness is derived, not stored.
	Liveness string `json:"liveness"`
}

// liveness classifies a session as of now.
func (s Session) liveness(now time.Time) string {
	if s.Status == StatusTerminal {
		return LiveTerminal
	}
	if now.Sub(s.LastSeen) > s.TTL {
		return LiveStale
	}
	return LiveRunning
}

// Store reads and writes session records.
type Store struct {
	db  *store.DB
	now func() time.Time
}

// New returns a Store using the wall clock.
func New(db *store.DB) *Store { return &Store{db: db, now: time.Now} }

// NewWithClock returns a Store reading time from now.
func NewWithClock(db *store.DB, now func() time.Time) *Store {
	return &Store{db: db, now: now}
}

// Record registers or updates the session for a role-task.
//
// Recording a different session over one that is still live is refused with a
// *LiveError naming the incumbent — that refusal is the duplicate-launch guard.
// Updating the same session id is always allowed and is how a heartbeat and a
// terminal status are both written.
//
// takeover overrides the refusal, for when the caller has proven the old
// process terminal by some means wfc cannot see.
func (s *Store) Record(ctx context.Context, in Session, takeover bool) (Session, error) {
	if in.Packet == "" || in.Role == "" {
		return Session{}, errors.New("sessions: a packet and a role are required")
	}
	if in.Status == "" {
		in.Status = StatusRunning
	}
	if in.Status != StatusRunning && in.Status != StatusTerminal {
		return Session{}, fmt.Errorf("sessions: status %q is not %s or %s",
			in.Status, StatusRunning, StatusTerminal)
	}
	if in.TTL <= 0 {
		in.TTL = DefaultTTL
	}
	now := s.now().UTC()
	in.LastSeen = now
	in.Started = now

	err := s.db.Tx(ctx, func(q *store.Queries) error {
		existing, found, err := get(ctx, q, in.Packet, in.Role, in.Task, in.Cycle, now)
		if err != nil {
			return err
		}
		if found {
			same := sameWorker(existing, in)
			if same {
				// A heartbeat or a terminal update: keep when it actually began.
				in.Started = existing.Started
			} else if existing.Liveness != LiveTerminal && !takeover {
				// Live or stale both refuse. A stale incumbent has only gone
				// quiet; wfc never observed it stop, so replacing it silently
				// would be exactly the duplicate launch this guards against.
				return &LiveError{Existing: existing}
			}
			if !same {
				// Displacing somebody's record of a worker has to be
				// attributable, so the displaced row is kept.
				replacedBy := in.SessionID
				if replacedBy == "" {
					replacedBy = in.AgentPath
				}
				note := "replaced"
				if takeover {
					note = "takeover"
				}
				if err := q.ArchiveWorkerSession(ctx, store.ArchiveWorkerSessionParams{
					PacketID:    existing.Packet,
					Role:        existing.Role,
					Task:        existing.Task,
					Cycle:       existing.Cycle,
					Client:      existing.Client,
					SessionID:   existing.SessionID,
					AgentPath:   existing.AgentPath,
					Status:      existing.Status,
					Liveness:    existing.Liveness,
					StartedAt:   existing.Started.Format(timeFormat),
					LastSeen:    existing.LastSeen.Format(timeFormat),
					ReplacedAt:  now.Format(timeFormat),
					ReplacedBy:  replacedBy,
					ReplaceNote: note,
				}); err != nil {
					return err
				}
			}
		}
		return q.PutWorkerSession(ctx, store.PutWorkerSessionParams{
			PacketID:   in.Packet,
			Role:       in.Role,
			Task:       in.Task,
			Cycle:      in.Cycle,
			Client:     in.Client,
			SessionID:  in.SessionID,
			AgentPath:  in.AgentPath,
			Parent:     in.Parent,
			Pid:        in.PID,
			Status:     in.Status,
			Reason:     in.Reason,
			Note:       in.Note,
			StartedAt:  in.Started.Format(timeFormat),
			LastSeen:   in.LastSeen.Format(timeFormat),
			TtlSeconds: int64(in.TTL / time.Second),
		})
	})
	if err != nil {
		return Session{}, err
	}
	in.TTLSeconds = int64(in.TTL / time.Second)
	in.Liveness = in.liveness(now)
	return in, nil
}

// List returns sessions, newest activity first within a packet. Passing an
// empty packet lists every packet; all includes terminal sessions.
func (s *Store) List(ctx context.Context, packet string, all bool) ([]Session, error) {
	now := s.now().UTC()

	var (
		rows []store.WorkerSession
		err  error
	)
	if packet == "" {
		rows, err = s.db.ListWorkerSessions(ctx)
	} else {
		rows, err = s.db.ListWorkerSessionsForPacket(ctx, packet)
	}
	if err != nil {
		return nil, err
	}

	out := make([]Session, 0, len(rows))
	for _, row := range rows {
		sess, ok := toSession(row, now)
		if !ok {
			continue
		}
		if !all && sess.Liveness == LiveTerminal {
			continue
		}
		out = append(out, sess)
	}
	return out, nil
}

// Get returns the session recorded for one role-task.
func (s *Store) Get(ctx context.Context, packet, role, task string, cycle int64) (Session, bool, error) {
	now := s.now().UTC()
	var (
		out   Session
		found bool
	)
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		var err error
		out, found, err = get(ctx, q, packet, role, task, cycle, now)
		return err
	})
	return out, found, err
}

func get(ctx context.Context, q *store.Queries, packet, role, task string, cycle int64, now time.Time) (Session, bool, error) {
	row, err := q.GetWorkerSession(ctx, store.GetWorkerSessionParams{
		PacketID: packet, Role: role, Task: task, Cycle: cycle,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, false, nil
		}
		return Session{}, false, err
	}
	sess, ok := toSession(row, now)
	return sess, ok, nil
}

func toSession(row store.WorkerSession, now time.Time) (Session, bool) {
	started, err := time.Parse(timeFormat, row.StartedAt)
	if err != nil {
		return Session{}, false
	}
	seen, err := time.Parse(timeFormat, row.LastSeen)
	if err != nil {
		return Session{}, false
	}
	s := Session{
		Packet:    row.PacketID,
		Role:      row.Role,
		Task:      row.Task,
		Cycle:     row.Cycle,
		Client:    row.Client,
		SessionID: row.SessionID,
		AgentPath: row.AgentPath,
		Parent:    row.Parent,
		PID:       row.Pid,
		Status:    row.Status,
		Reason:    row.Reason,
		Note:      row.Note,
		Started:   started.UTC(),
		LastSeen:  seen.UTC(),
		TTL:       time.Duration(row.TtlSeconds) * time.Second,
	}
	s.TTLSeconds = int64(s.TTL / time.Second)
	s.Liveness = s.liveness(now)
	return s, true
}
