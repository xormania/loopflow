// Package api defines the wire between a loopflow client and a loopflow
// server. The protocol mirrors the store operations one to one so local and
// remote execution cannot drift: the same request shapes, the same typed
// errors, the same exit codes on the other side.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/xormania/loopflow/internal/artifacts"
	"github.com/xormania/loopflow/internal/attempts"
	"github.com/xormania/loopflow/internal/claims"
	"github.com/xormania/loopflow/internal/events"
	"github.com/xormania/loopflow/internal/sessions"
	"github.com/xormania/loopflow/internal/store"
)

// Version is the build identity, shared by the CLI and the wire so a client
// and server from different builds refuse to talk rather than half-work.
const Version = "0.1.0"

// Headers. The project travels with every request — identity belongs to the
// work, not to the endpoint.
const (
	HeaderVersion       = "X-Loopflow-Version"
	HeaderProjectKey    = "X-Loopflow-Project-Key"
	HeaderProjectName   = "X-Loopflow-Project-Name"
	HeaderProjectSource = "X-Loopflow-Project-Source"
)

// Requests and responses, one pair per operation.

type CreatePacketRequest struct {
	Packet    string `json:"packet"`
	Objective string `json:"objective"`
}

type AppendRequest struct {
	Packet string         `json:"packet"`
	Event  events.Event   `json:"event"`
	State  map[string]any `json:"state,omitempty"`
}

type AppendResponse struct {
	Event events.Event `json:"event"`
}

type PacketRequest struct {
	Packet string `json:"packet"`
}

type ProjectionResponse struct {
	Projection *events.Projection `json:"projection"`
}

type PacketResponse struct {
	Packet store.Packet `json:"packet"`
}

type ListPacketsResponse struct {
	Packets []store.Packet `json:"packets"`
}

type CountResponse struct {
	Count int64 `json:"count"`
}

type ListEventsResponse struct {
	Events []store.Event `json:"events"`
}

type EventResponse struct {
	Event store.Event `json:"event"`
}

type AcquireRequest struct {
	Packet string `json:"packet"`
	Owner  string `json:"owner"`
	Note   string `json:"note,omitempty"`
	TTLMS  int64  `json:"ttl_ms"`
}

type ClaimResponse struct {
	Claim claims.Claim `json:"claim"`
}

type ReleaseRequest struct {
	Packet string `json:"packet"`
	Owner  string `json:"owner"`
}

type SessionRecordRequest struct {
	Session  sessions.Session `json:"session"`
	Takeover bool             `json:"takeover"`
}

type SessionResponse struct {
	Session sessions.Session `json:"session"`
}

type SessionListRequest struct {
	Packet string `json:"packet"`
	All    bool   `json:"all"`
}

type SessionListResponse struct {
	Sessions []sessions.Session `json:"sessions"`
}

type AttemptRecordRequest struct {
	Outcome attempts.Outcome `json:"outcome"`
}

type AttemptResponse struct {
	Attempt attempts.Attempt `json:"attempt"`
}

type AttemptListRequest struct {
	Packet  string            `json:"packet"`
	Current attempts.Bindings `json:"current"`
}

type AttemptListResponse struct {
	Attempts []attempts.Attempt `json:"attempts"`
}

type DescriptorResponse struct {
	Descriptor artifacts.Descriptor `json:"descriptor"`
}

type ProjectRow struct {
	Key    string `json:"key"`
	Name   string `json:"name,omitempty"`
	Source string `json:"source,omitempty"`
}

type ProjectsResponse struct {
	Projects []ProjectRow `json:"projects"`
}

// Error kinds. Every typed error the stores produce must survive the wire, or
// remote exit codes and refusal texts would diverge from local ones.
const (
	KindRefusal           = "refusal"
	KindIntegrity         = "event-integrity"
	KindArtifactIntegrity = "artifact-integrity"
	KindHeld              = "claim-held"
	KindSessionLive       = "session-live"
	KindNotAChain         = "not-a-chain"
	KindNoRows            = "no-rows"
	KindDigestMismatch    = "digest-mismatch"
	KindPlain             = "plain"
)

// WireError is the error envelope. Exactly one payload field is set for the
// typed kinds; Message always carries the rendered text.
type WireError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`

	Refusal   *RefusalPayload   `json:"refusal,omitempty"`
	Integrity *IntegrityPayload `json:"integrity,omitempty"`
	Artifact  *ArtifactPayload  `json:"artifact,omitempty"`
	Held      *HeldPayload      `json:"held,omitempty"`
	Live      *sessions.Session `json:"live,omitempty"`
}

type RefusalPayload struct {
	PacketID     string `json:"packet_id"`
	Precondition string `json:"precondition"`
	Expected     string `json:"expected,omitempty"`
	Actual       string `json:"actual,omitempty"`
	Needed       string `json:"needed,omitempty"`
}

type IntegrityPayload struct {
	PacketID string `json:"packet_id"`
	Seq      int64  `json:"seq,omitempty"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail,omitempty"`
}

type ArtifactPayload struct {
	Digest string `json:"digest"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

type HeldPayload struct {
	Packet  string    `json:"packet"`
	Owner   string    `json:"owner"`
	Expires time.Time `json:"expires"`
}

// FromError classifies err into a WireError, in the same order the CLI's own
// exit-code mapping tests errors — that ordering is the contract.
func FromError(err error) *WireError {
	w := &WireError{Kind: KindPlain, Message: err.Error()}

	var live *sessions.LiveError
	var held *claims.HeldError
	var eventIntegrity *events.IntegrityError
	var artifactIntegrity *artifacts.IntegrityError
	var refusal *events.RefusalError
	switch {
	case errors.As(err, &live):
		w.Kind = KindSessionLive
		s := live.Existing
		w.Live = &s
	case errors.As(err, &held):
		w.Kind = KindHeld
		w.Held = &HeldPayload{Packet: held.Packet, Owner: held.Owner, Expires: held.Expires}
	case errors.As(err, &eventIntegrity):
		w.Kind = KindIntegrity
		w.Integrity = &IntegrityPayload{
			PacketID: eventIntegrity.PacketID,
			Seq:      eventIntegrity.Seq,
			Reason:   eventIntegrity.Reason,
			Detail:   eventIntegrity.Detail,
		}
	case errors.As(err, &artifactIntegrity):
		w.Kind = KindArtifactIntegrity
		w.Artifact = &ArtifactPayload{
			Digest: artifactIntegrity.Digest,
			Reason: artifactIntegrity.Reason,
			Detail: artifactIntegrity.Detail,
		}
	case errors.As(err, &refusal):
		w.Kind = KindRefusal
		w.Refusal = &RefusalPayload{
			PacketID:     refusal.PacketID,
			Precondition: refusal.Precondition,
			Expected:     refusal.Expected,
			Actual:       refusal.Actual,
			Needed:       refusal.Needed,
		}
	case errors.Is(err, events.ErrNotAChain):
		w.Kind = KindNotAChain
	case errors.Is(err, artifacts.ErrDigestMismatch):
		w.Kind = KindDigestMismatch
	case errors.Is(err, sql.ErrNoRows):
		w.Kind = KindNoRows
	}
	return w
}

// Reconstruct turns a WireError back into the typed error the stores would
// have returned locally, so the client's exit-code mapping needs no special
// remote cases.
func (w *WireError) Reconstruct() error {
	switch w.Kind {
	case KindSessionLive:
		if w.Live != nil {
			return &sessions.LiveError{Existing: *w.Live}
		}
	case KindHeld:
		if w.Held != nil {
			return &claims.HeldError{Packet: w.Held.Packet, Owner: w.Held.Owner, Expires: w.Held.Expires}
		}
	case KindIntegrity:
		if w.Integrity != nil {
			return &events.IntegrityError{
				PacketID: w.Integrity.PacketID,
				Seq:      w.Integrity.Seq,
				Reason:   w.Integrity.Reason,
				Detail:   w.Integrity.Detail,
			}
		}
	case KindArtifactIntegrity:
		if w.Artifact != nil {
			return &artifacts.IntegrityError{
				Digest: w.Artifact.Digest,
				Reason: w.Artifact.Reason,
				Detail: w.Artifact.Detail,
			}
		}
	case KindRefusal:
		if w.Refusal != nil {
			return &events.RefusalError{
				PacketID:     w.Refusal.PacketID,
				Precondition: w.Refusal.Precondition,
				Expected:     w.Refusal.Expected,
				Actual:       w.Refusal.Actual,
				Needed:       w.Refusal.Needed,
			}
		}
	case KindNotAChain:
		return fmt.Errorf("%s: %w", w.Message, events.ErrNotAChain)
	case KindDigestMismatch:
		return artifacts.ErrDigestMismatch
	case KindNoRows:
		return sql.ErrNoRows
	}
	return errors.New(w.Message)
}

// DecodeJSON decodes with UseNumber. Events and packet state ride the wire as
// canonical-model values: a float64 sneaking in where a json.Number was would
// re-hash to different bytes and turn intact evidence into a false integrity
// failure.
func DecodeJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("api: decode: %w", err)
	}
	return nil
}
