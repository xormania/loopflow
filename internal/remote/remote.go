// Package remote implements the backend interfaces against a loopflow
// server. Commands cannot tell it from the local store — same operations,
// same typed errors reconstructed from the wire, same exit codes.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xormania/loopflow/internal/api"
	"github.com/xormania/loopflow/internal/artifacts"
	"github.com/xormania/loopflow/internal/attempts"
	"github.com/xormania/loopflow/internal/backend"
	"github.com/xormania/loopflow/internal/claims"
	"github.com/xormania/loopflow/internal/events"
	"github.com/xormania/loopflow/internal/sessions"
	"github.com/xormania/loopflow/internal/stateroot"
	"github.com/xormania/loopflow/internal/store"
)

type client struct {
	base    string
	token   string
	project stateroot.Project
	http    *http.Client
}

// Dial builds a backend that executes every operation against the server at
// baseURL, carrying the given project identity on each request.
func Dial(baseURL, token string, project stateroot.Project) (*backend.Backend, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("remote: %q is not a server URL", baseURL)
	}
	c := &client{
		base:    strings.TrimRight(baseURL, "/"),
		token:   token,
		project: project,
		http:    &http.Client{},
	}
	return &backend.Backend{
		Events:    (*eventsClient)(c),
		Queries:   (*queriesClient)(c),
		Claims:    (*claimsClient)(c),
		Sessions:  (*sessionsClient)(c),
		Attempts:  (*attemptsClient)(c),
		Artifacts: (*artifactsClient)(c),
		Projects:  c.projects,
	}, nil
}

func (c *client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("remote: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(api.HeaderVersion, api.Version)
	req.Header.Set(api.HeaderProjectKey, c.project.Key)
	req.Header.Set(api.HeaderProjectName, c.project.Name)
	req.Header.Set(api.HeaderProjectSource, c.project.Source)
	return req, nil
}

// checkError decodes a non-200 response into the typed error it carries.
func checkError(resp *http.Response) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	defer resp.Body.Close()
	var w api.WireError
	if err := api.DecodeJSON(io.LimitReader(resp.Body, 1<<20), &w); err != nil {
		return fmt.Errorf("remote: server returned %s", resp.Status)
	}
	return w.Reconstruct()
}

// call posts a JSON request and decodes a JSON response.
func (c *client) call(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("remote: encode request: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("remote: %s unreachable: %w", c.base, err)
	}
	if err := checkError(resp); err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return api.DecodeJSON(resp.Body, out)
}

func (c *client) projects(ctx context.Context) ([]api.ProjectRow, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/projects", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("remote: %s unreachable: %w", c.base, err)
	}
	if err := checkError(resp); err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out api.ProjectsResponse
	if err := api.DecodeJSON(resp.Body, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

type eventsClient client

func (c *eventsClient) CreatePacket(ctx context.Context, packetID, objective string) error {
	return (*client)(c).call(ctx, "/v1/packets.create",
		api.CreatePacketRequest{Packet: packetID, Objective: objective}, nil)
}

func (c *eventsClient) Append(ctx context.Context, packetID string, in events.Event, state map[string]any) (events.Event, error) {
	var out api.AppendResponse
	err := (*client)(c).call(ctx, "/v1/events.append",
		api.AppendRequest{Packet: packetID, Event: in, State: state}, &out)
	return out.Event, err
}

func (c *eventsClient) VerifyChain(ctx context.Context, packetID string) error {
	return (*client)(c).call(ctx, "/v1/events.verify", api.PacketRequest{Packet: packetID}, nil)
}

func (c *eventsClient) Project(ctx context.Context, packetID string) (*events.Projection, error) {
	var out api.ProjectionResponse
	err := (*client)(c).call(ctx, "/v1/events.project", api.PacketRequest{Packet: packetID}, &out)
	return out.Projection, err
}

type queriesClient client

func (c *queriesClient) GetPacket(ctx context.Context, packetID string) (store.Packet, error) {
	var out api.PacketResponse
	err := (*client)(c).call(ctx, "/v1/packets.get", api.PacketRequest{Packet: packetID}, &out)
	return out.Packet, err
}

func (c *queriesClient) ListPackets(ctx context.Context) ([]store.Packet, error) {
	var out api.ListPacketsResponse
	err := (*client)(c).call(ctx, "/v1/packets.list", struct{}{}, &out)
	return out.Packets, err
}

func (c *queriesClient) CountEvents(ctx context.Context, packetID string) (int64, error) {
	var out api.CountResponse
	err := (*client)(c).call(ctx, "/v1/events.count", api.PacketRequest{Packet: packetID}, &out)
	return out.Count, err
}

func (c *queriesClient) ListEvents(ctx context.Context, packetID string) ([]store.Event, error) {
	var out api.ListEventsResponse
	err := (*client)(c).call(ctx, "/v1/events.list", api.PacketRequest{Packet: packetID}, &out)
	return out.Events, err
}

func (c *queriesClient) GetChainTail(ctx context.Context, packetID string) (store.Event, error) {
	var out api.EventResponse
	err := (*client)(c).call(ctx, "/v1/events.tail", api.PacketRequest{Packet: packetID}, &out)
	return out.Event, err
}

type claimsClient client

func (c *claimsClient) Acquire(ctx context.Context, packet, owner, note string, ttl time.Duration) (claims.Claim, error) {
	var out api.ClaimResponse
	err := (*client)(c).call(ctx, "/v1/claims.acquire", api.AcquireRequest{
		Packet: packet, Owner: owner, Note: note, TTLMS: ttl.Milliseconds(),
	}, &out)
	return out.Claim, err
}

func (c *claimsClient) Release(ctx context.Context, packet, owner string) error {
	return (*client)(c).call(ctx, "/v1/claims.release", api.ReleaseRequest{Packet: packet, Owner: owner}, nil)
}

type sessionsClient client

func (c *sessionsClient) Record(ctx context.Context, in sessions.Session, takeover bool) (sessions.Session, error) {
	// The duration is not serialized; seconds are. The server reconstructs
	// the duration and stamps deadlines on its own clock.
	if in.TTLSeconds == 0 && in.TTL > 0 {
		in.TTLSeconds = int64(in.TTL / time.Second)
	}
	var out api.SessionResponse
	err := (*client)(c).call(ctx, "/v1/sessions.record",
		api.SessionRecordRequest{Session: in, Takeover: takeover}, &out)
	return out.Session, err
}

func (c *sessionsClient) List(ctx context.Context, packet string, all bool) ([]sessions.Session, error) {
	var out api.SessionListResponse
	err := (*client)(c).call(ctx, "/v1/sessions.list", api.SessionListRequest{Packet: packet, All: all}, &out)
	return out.Sessions, err
}

type attemptsClient client

func (c *attemptsClient) Record(ctx context.Context, o attempts.Outcome) (attempts.Attempt, error) {
	var out api.AttemptResponse
	err := (*client)(c).call(ctx, "/v1/attempts.record", api.AttemptRecordRequest{Outcome: o}, &out)
	return out.Attempt, err
}

func (c *attemptsClient) List(ctx context.Context, packet string, current attempts.Bindings) ([]attempts.Attempt, error) {
	var out api.AttemptListResponse
	err := (*client)(c).call(ctx, "/v1/attempts.list",
		api.AttemptListRequest{Packet: packet, Current: current}, &out)
	return out.Attempts, err
}

type artifactsClient client

func (c *artifactsClient) put(ctx context.Context, r io.Reader, expected string, meta artifacts.Meta) (artifacts.Descriptor, error) {
	q := url.Values{}
	q.Set("class", meta.Class)
	q.Set("media_type", meta.MediaType)
	if expected != "" {
		q.Set("expect", expected)
	}
	req, err := (*client)(c).newRequest(ctx, http.MethodPost, "/v1/artifacts.put?"+q.Encode(), r)
	if err != nil {
		return artifacts.Descriptor{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return artifacts.Descriptor{}, fmt.Errorf("remote: %s unreachable: %w", c.base, err)
	}
	if err := checkError(resp); err != nil {
		return artifacts.Descriptor{}, err
	}
	defer resp.Body.Close()
	var out api.DescriptorResponse
	if err := api.DecodeJSON(resp.Body, &out); err != nil {
		return artifacts.Descriptor{}, err
	}
	return out.Descriptor, nil
}

func (c *artifactsClient) Put(ctx context.Context, r io.Reader, meta artifacts.Meta) (artifacts.Descriptor, error) {
	return c.put(ctx, r, "", meta)
}

func (c *artifactsClient) PutExpected(ctx context.Context, r io.Reader, expected string, meta artifacts.Meta) (artifacts.Descriptor, error) {
	return c.put(ctx, r, expected, meta)
}

func (c *artifactsClient) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	req, err := (*client)(c).newRequest(ctx, http.MethodGet,
		"/v1/artifacts.get?digest="+url.QueryEscape(digest), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("remote: %s unreachable: %w", c.base, err)
	}
	if err := checkError(resp); err != nil {
		return nil, err
	}
	return resp.Body, nil
}
