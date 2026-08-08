// Package server exposes a state root over HTTP so harnesses in other
// containers can reach the same coordination state the local CLI uses.
//
// The server is project-agnostic: identity travels with each request, and a
// project's slice of the root is created on first use exactly as it is
// locally. It accepts records and answers queries; it assigns nothing,
// launches nothing, and decides nothing — the same boundary as the store it
// fronts, now with one clock: TTLs are computed here, so container clock skew
// cannot corrupt expiry.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/xormania/loopflow/internal/api"
	"github.com/xormania/loopflow/internal/artifacts"
	"github.com/xormania/loopflow/internal/backend"
	"github.com/xormania/loopflow/internal/stateroot"
	"github.com/xormania/loopflow/internal/store"
)

// Server serves every project under one state root.
type Server struct {
	root  string
	token string

	mu       sync.Mutex
	backends map[string]*backend.Backend
}

// New builds a server for root. The token is required on every request; an
// empty token refuses to serve rather than serving openly.
func New(root, token string) (*Server, error) {
	if token == "" {
		return nil, errors.New("server: refusing to serve without a token")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("server: resolve root %q: %w", root, err)
	}
	return &Server{root: abs, token: token, backends: map[string]*backend.Backend{}}, nil
}

// Close closes every project backend the server opened.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.backends {
		_ = b.Close()
	}
	s.backends = map[string]*backend.Backend{}
}

// project resolves the request's project and its backend, opening and
// migrating the project database on first use.
func (s *Server) project(r *http.Request) (*backend.Backend, error) {
	key := r.Header.Get(api.HeaderProjectKey)
	p := stateroot.Project{
		Key:    key,
		Name:   r.Header.Get(api.HeaderProjectName),
		Source: r.Header.Get(api.HeaderProjectSource),
	}
	if !stateroot.ValidKey(key) {
		return nil, fmt.Errorf("server: invalid project key %q", key)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.backends[key]; ok {
		return b, nil
	}

	layout, err := stateroot.New(s.root, p)
	if err != nil {
		return nil, err
	}
	if _, err := layout.Ensure(); err != nil {
		return nil, err
	}
	db, err := store.Open(r.Context(), layout.Database)
	if err != nil {
		return nil, err
	}
	if _, err := db.Migrate(r.Context()); err != nil {
		_ = db.Close()
		return nil, err
	}
	b, err := backend.Local(db, layout.Artifacts, s.listProjects)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.backends[key] = b
	return b, nil
}

func (s *Server) listProjects(ctx context.Context) ([]api.ProjectRow, error) {
	return ListProjects(s.root)
}

// ListProjects reads the project markers under a state root.
func ListProjects(root string) ([]api.ProjectRow, error) {
	entries, err := os.ReadDir(filepath.Join(root, "projects"))
	if err != nil {
		if os.IsNotExist(err) {
			return []api.ProjectRow{}, nil
		}
		return nil, err
	}
	rows := []api.ProjectRow{}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name, source, _ := stateroot.ReadMarker(filepath.Join(root, "projects", ent.Name()))
		rows = append(rows, api.ProjectRow{Key: ent.Name(), Name: name, Source: source})
	}
	return rows, nil
}

// Handler returns the HTTP surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/packets.create", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.CreatePacketRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		return struct{}{}, b.Events.CreatePacket(r.Context(), req.Packet, req.Objective)
	}))
	mux.HandleFunc("POST /v1/events.append", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.AppendRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		ev, err := b.Events.Append(r.Context(), req.Packet, req.Event, req.State)
		return api.AppendResponse{Event: ev}, err
	}))
	mux.HandleFunc("POST /v1/events.verify", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.PacketRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		return struct{}{}, b.Events.VerifyChain(r.Context(), req.Packet)
	}))
	mux.HandleFunc("POST /v1/events.project", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.PacketRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		p, err := b.Events.Project(r.Context(), req.Packet)
		return api.ProjectionResponse{Projection: p}, err
	}))

	mux.HandleFunc("POST /v1/packets.get", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.PacketRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		p, err := b.Queries.GetPacket(r.Context(), req.Packet)
		return api.PacketResponse{Packet: p}, err
	}))
	mux.HandleFunc("POST /v1/packets.list", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		ps, err := b.Queries.ListPackets(r.Context())
		return api.ListPacketsResponse{Packets: ps}, err
	}))
	mux.HandleFunc("POST /v1/events.count", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.PacketRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		n, err := b.Queries.CountEvents(r.Context(), req.Packet)
		return api.CountResponse{Count: n}, err
	}))
	mux.HandleFunc("POST /v1/events.list", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.PacketRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		evs, err := b.Queries.ListEvents(r.Context(), req.Packet)
		return api.ListEventsResponse{Events: evs}, err
	}))
	mux.HandleFunc("POST /v1/events.tail", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.PacketRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		ev, err := b.Queries.GetChainTail(r.Context(), req.Packet)
		return api.EventResponse{Event: ev}, err
	}))

	mux.HandleFunc("POST /v1/claims.acquire", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.AcquireRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		c, err := b.Claims.Acquire(r.Context(), req.Packet, req.Owner, req.Note,
			time.Duration(req.TTLMS)*time.Millisecond)
		return api.ClaimResponse{Claim: c}, err
	}))
	mux.HandleFunc("POST /v1/claims.release", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.ReleaseRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		return struct{}{}, b.Claims.Release(r.Context(), req.Packet, req.Owner)
	}))

	mux.HandleFunc("POST /v1/sessions.record", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.SessionRecordRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		// TTL rides the wire in seconds; the duration is reconstructed here
		// and every deadline is computed on this server's clock.
		req.Session.TTL = time.Duration(req.Session.TTLSeconds) * time.Second
		sess, err := b.Sessions.Record(r.Context(), req.Session, req.Takeover)
		return api.SessionResponse{Session: sess}, err
	}))
	mux.HandleFunc("POST /v1/sessions.list", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.SessionListRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		list, err := b.Sessions.List(r.Context(), req.Packet, req.All)
		return api.SessionListResponse{Sessions: list}, err
	}))

	mux.HandleFunc("POST /v1/attempts.record", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.AttemptRecordRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		a, err := b.Attempts.Record(r.Context(), req.Outcome)
		return api.AttemptResponse{Attempt: a}, err
	}))
	mux.HandleFunc("POST /v1/attempts.list", s.typed(func(b *backend.Backend, r *http.Request) (any, error) {
		var req api.AttemptListRequest
		if err := api.DecodeJSON(r.Body, &req); err != nil {
			return nil, err
		}
		list, err := b.Attempts.List(r.Context(), req.Packet, req.Current)
		return api.AttemptListResponse{Attempts: list}, err
	}))

	mux.HandleFunc("POST /v1/artifacts.put", s.authed(func(w http.ResponseWriter, r *http.Request) {
		b, err := s.project(r)
		if err != nil {
			writeError(w, err)
			return
		}
		meta := artifacts.Meta{
			Class:     r.URL.Query().Get("class"),
			MediaType: r.URL.Query().Get("media_type"),
		}
		var d artifacts.Descriptor
		if expect := r.URL.Query().Get("expect"); expect != "" {
			d, err = b.Artifacts.PutExpected(r.Context(), r.Body, expect, meta)
		} else {
			d, err = b.Artifacts.Put(r.Context(), r.Body, meta)
		}
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, api.DescriptorResponse{Descriptor: d})
	}))
	mux.HandleFunc("GET /v1/artifacts.get", s.authed(func(w http.ResponseWriter, r *http.Request) {
		b, err := s.project(r)
		if err != nil {
			writeError(w, err)
			return
		}
		rc, err := b.Artifacts.Get(r.Context(), r.URL.Query().Get("digest"))
		if err != nil {
			writeError(w, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, rc)
	}))

	mux.HandleFunc("GET /v1/projects", s.authed(func(w http.ResponseWriter, r *http.Request) {
		rows, err := ListProjects(s.root)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, api.ProjectsResponse{Projects: rows})
	}))

	return mux
}

// typed wraps a JSON-in/JSON-out handler with auth and project resolution.
func (s *Server) typed(fn func(*backend.Backend, *http.Request) (any, error)) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		b, err := s.project(r)
		if err != nil {
			writeError(w, err)
			return
		}
		resp, err := fn(b, r)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// authed enforces the bearer token and the version handshake.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get(api.HeaderVersion); v != api.Version {
			writeJSON(w, http.StatusConflict, api.WireError{
				Kind:    api.KindPlain,
				Message: fmt.Sprintf("server: version mismatch: server %s, client %s", api.Version, v),
			})
			return
		}
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.token
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writeJSON(w, http.StatusUnauthorized, api.WireError{
				Kind:    api.KindPlain,
				Message: "server: missing or wrong token",
			})
			return
		}
		next(w, r)
	}
}

func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusUnprocessableEntity, api.FromError(err))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
