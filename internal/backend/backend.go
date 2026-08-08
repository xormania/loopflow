// Package backend is the seam between commands and where their state lives.
// The CLI talks to these interfaces only; a local store and a remote server
// implement the same surface, so every command works identically against
// either and cannot know which it got.
package backend

import (
	"context"
	"io"
	"time"

	"github.com/xormania/loopflow/internal/api"
	"github.com/xormania/loopflow/internal/artifacts"
	"github.com/xormania/loopflow/internal/attempts"
	"github.com/xormania/loopflow/internal/claims"
	"github.com/xormania/loopflow/internal/events"
	"github.com/xormania/loopflow/internal/sessions"
	"github.com/xormania/loopflow/internal/store"
)

// Events is the packet event chain.
type Events interface {
	CreatePacket(ctx context.Context, packetID, objective string) error
	Append(ctx context.Context, packetID string, in events.Event, state map[string]any) (events.Event, error)
	VerifyChain(ctx context.Context, packetID string) error
	Project(ctx context.Context, packetID string) (*events.Projection, error)
}

// Queries are the read-only lookups commands use directly.
type Queries interface {
	GetPacket(ctx context.Context, packetID string) (store.Packet, error)
	ListPackets(ctx context.Context) ([]store.Packet, error)
	CountEvents(ctx context.Context, packetID string) (int64, error)
	ListEvents(ctx context.Context, packetID string) ([]store.Event, error)
	GetChainTail(ctx context.Context, packetID string) (store.Event, error)
}

// Claims arbitrates packet custody.
type Claims interface {
	Acquire(ctx context.Context, packet, owner, note string, ttl time.Duration) (claims.Claim, error)
	Release(ctx context.Context, packet, owner string) error
}

// Sessions is the provider-session registry.
type Sessions interface {
	Record(ctx context.Context, in sessions.Session, takeover bool) (sessions.Session, error)
	List(ctx context.Context, packet string, all bool) ([]sessions.Session, error)
}

// Attempts records what was tried.
type Attempts interface {
	Record(ctx context.Context, o attempts.Outcome) (attempts.Attempt, error)
	List(ctx context.Context, packet string, current attempts.Bindings) ([]attempts.Attempt, error)
}

// Artifacts is the content-addressed store.
type Artifacts interface {
	Put(ctx context.Context, r io.Reader, meta artifacts.Meta) (artifacts.Descriptor, error)
	PutExpected(ctx context.Context, r io.Reader, expected string, meta artifacts.Meta) (artifacts.Descriptor, error)
	Get(ctx context.Context, digest string) (io.ReadCloser, error)
}

// Backend bundles one project's stores, however they are reached.
type Backend struct {
	Events    Events
	Queries   Queries
	Claims    Claims
	Sessions  Sessions
	Attempts  Attempts
	Artifacts Artifacts

	// Projects lists what the state root knows; on a remote backend that is
	// the server's root, which is the point.
	Projects func(ctx context.Context) ([]api.ProjectRow, error)

	closeFn func() error
}

// Close releases whatever the backend holds.
func (b *Backend) Close() error {
	if b == nil || b.closeFn == nil {
		return nil
	}
	return b.closeFn()
}

// Local builds a backend on an opened, migrated database and its artifact
// directory. Closing the backend closes the database.
func Local(db *store.DB, artifactsRoot string, projects func(ctx context.Context) ([]api.ProjectRow, error)) (*Backend, error) {
	art, err := artifacts.Open(db, artifactsRoot)
	if err != nil {
		return nil, err
	}
	return &Backend{
		Events:    events.New(db),
		Queries:   db,
		Claims:    claims.New(db),
		Sessions:  sessions.New(db),
		Attempts:  attempts.New(db),
		Artifacts: art,
		Projects:  projects,
		closeFn:   db.Close,
	}, nil
}
