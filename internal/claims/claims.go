// Package claims is how peer harnesses avoid picking up the same work.
//
// There is no coordinator: several harnesses read and write the same store,
// each deciding for itself what to do next. A claim is the fact that stops two
// of them starting the same packet. Acquiring one is a compare-and-swap inside
// a single write transaction, so exactly one caller can win a contested
// packet, however many are racing.
//
// Claims expire. A harness that dies mid-task would otherwise hold its packet
// forever, and a loop that can only be unwedged by hand is not much of a loop.
// Re-acquiring a claim you already hold extends it, so a long task heartbeats
// by claiming again rather than by a separate call.
package claims

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xormania/wfc/internal/store"
)

// DefaultTTL is how long a claim lasts when the caller does not say.
const DefaultTTL = 15 * time.Minute

// timeFormat is fixed-width so that string comparison in SQL is chronological
// comparison. RFC3339Nano trims trailing zeros, which would sort
// "…:00.5Z" before "…:00Z" and quietly break expiry queries.
const timeFormat = "2006-01-02T15:04:05.000000Z"

// ErrHeld reports that another owner holds a live claim.
var ErrHeld = errors.New("claim is held by another owner")

// HeldError says who holds a packet and until when, so the caller can decide
// whether to wait or move on.
type HeldError struct {
	Packet  string
	Owner   string
	Expires time.Time
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("packet %s is held by %s until %s",
		e.Packet, e.Owner, e.Expires.Format(timeFormat))
}

func (e *HeldError) Unwrap() []error { return []error{ErrHeld} }

// Claim is one harness's hold on one packet.
type Claim struct {
	Packet   string    `json:"packet"`
	Owner    string    `json:"owner"`
	Note     string    `json:"note,omitempty"`
	Acquired time.Time `json:"acquired"`
	Expires  time.Time `json:"expires"`
}

// Store reads and writes claims.
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

// Acquire takes the claim on a packet.
//
// It succeeds when nobody holds it, when the existing claim has expired, or
// when the caller already holds it — in which case the claim is extended. It
// fails with a *HeldError when a different owner holds a live claim.
func (s *Store) Acquire(ctx context.Context, packet, owner, note string, ttl time.Duration) (Claim, error) {
	if owner == "" {
		return Claim{}, errors.New("claims: an owner is required")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	now := s.now().UTC()

	var out Claim
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		if _, err := q.GetPacket(ctx, packet); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("claims: no such packet %q", packet)
			}
			return err
		}
		if held, err := liveClaim(ctx, q, packet, now); err != nil {
			return err
		} else if held != nil && held.Owner != owner {
			return &HeldError{Packet: packet, Owner: held.Owner, Expires: held.Expires}
		}

		out = Claim{
			Packet:   packet,
			Owner:    owner,
			Note:     note,
			Acquired: now,
			Expires:  now.Add(ttl),
		}
		return q.PutClaim(ctx, store.PutClaimParams{
			PacketID:   packet,
			Owner:      owner,
			Note:       note,
			AcquiredAt: now.Format(timeFormat),
			ExpiresAt:  out.Expires.Format(timeFormat),
		})
	})
	if err != nil {
		return Claim{}, err
	}
	return out, nil
}

// Release drops a claim. Releasing a packet nobody holds is not an error;
// releasing one somebody else holds is.
func (s *Store) Release(ctx context.Context, packet, owner string) error {
	if owner == "" {
		return errors.New("claims: an owner is required")
	}
	now := s.now().UTC()

	return s.db.Tx(ctx, func(q *store.Queries) error {
		held, err := liveClaim(ctx, q, packet, now)
		if err != nil {
			return err
		}
		if held != nil && held.Owner != owner {
			return &HeldError{Packet: packet, Owner: held.Owner, Expires: held.Expires}
		}
		return q.DeleteClaim(ctx, packet)
	})
}

// Next claims the oldest packet nobody holds, so a harness can pull work
// without first asking what work there is. It reports false when there is
// nothing available.
func (s *Store) Next(ctx context.Context, owner, note string, ttl time.Duration) (Claim, bool, error) {
	if owner == "" {
		return Claim{}, false, errors.New("claims: an owner is required")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	now := s.now().UTC()

	var out Claim
	found := false
	// The scan and the claim happen in one transaction, so two harnesses
	// pulling at the same moment cannot be handed the same packet.
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		available, err := q.ListUnclaimedPackets(ctx, now.Format(timeFormat))
		if err != nil {
			return err
		}
		if len(available) == 0 {
			return nil
		}
		packet := available[0]
		out = Claim{
			Packet:   packet,
			Owner:    owner,
			Note:     note,
			Acquired: now,
			Expires:  now.Add(ttl),
		}
		found = true
		return q.PutClaim(ctx, store.PutClaimParams{
			PacketID:   packet,
			Owner:      owner,
			Note:       note,
			AcquiredAt: now.Format(timeFormat),
			ExpiresAt:  out.Expires.Format(timeFormat),
		})
	})
	if err != nil {
		return Claim{}, false, err
	}
	return out, found, nil
}

// Get returns the live claim on a packet, if there is one. An expired claim
// reports as absent: it no longer stops anybody.
func (s *Store) Get(ctx context.Context, packet string) (Claim, bool, error) {
	now := s.now().UTC()
	var (
		out   Claim
		found bool
	)
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		held, err := liveClaim(ctx, q, packet, now)
		if err != nil {
			return err
		}
		if held != nil {
			out, found = *held, true
		}
		return nil
	})
	return out, found, err
}

// List returns every live claim.
func (s *Store) List(ctx context.Context) ([]Claim, error) {
	now := s.now().UTC()
	rows, err := s.db.ListClaims(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Claim, 0, len(rows))
	for _, row := range rows {
		c, ok := toClaim(row)
		if !ok || !c.Expires.After(now) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// liveClaim returns the claim on a packet when one exists and has not expired.
func liveClaim(ctx context.Context, q *store.Queries, packet string, now time.Time) (*Claim, error) {
	row, err := q.GetClaim(ctx, packet)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	c, ok := toClaim(row)
	if !ok || !c.Expires.After(now) {
		return nil, nil
	}
	return &c, nil
}

func toClaim(row store.Claim) (Claim, bool) {
	acquired, err := time.Parse(timeFormat, row.AcquiredAt)
	if err != nil {
		return Claim{}, false
	}
	expires, err := time.Parse(timeFormat, row.ExpiresAt)
	if err != nil {
		return Claim{}, false
	}
	return Claim{
		Packet:   row.PacketID,
		Owner:    row.Owner,
		Note:     row.Note,
		Acquired: acquired.UTC(),
		Expires:  expires.UTC(),
	}, true
}
