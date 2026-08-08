package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/xormania/loopflow/internal/canonical"
	"github.com/xormania/loopflow/internal/store"
)

// Classification values. The full taxonomy per spec 03 arrives with the
// lifecycle gates in Phase 5; these are what the artifact store can produce.
const (
	ClassEvidenceIntegrity = "evidence-integrity"
)

var (
	// ErrEvidenceIntegrity marks stored content that does not match what the
	// database records about it. Every IntegrityError matches it.
	ErrEvidenceIntegrity = errors.New(ClassEvidenceIntegrity)

	// ErrNotFound is returned for a digest the store does not hold.
	ErrNotFound = errors.New("artifacts: no such artifact")

	// ErrDigestMismatch is returned by PutExpected when the content does not
	// hash to the digest the caller declared.
	ErrDigestMismatch = errors.New("artifacts: content does not match the expected digest")
)

// IntegrityError reports that stored content does not match its record.
type IntegrityError struct {
	Digest string
	Reason string
	Detail string
	err    error
}

func (e *IntegrityError) Error() string {
	msg := fmt.Sprintf("%s: artifact %s: %s", ClassEvidenceIntegrity, e.Digest, e.Reason)
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	return msg
}

// Classification reports the failure class this error belongs to.
func (e *IntegrityError) Classification() string { return ClassEvidenceIntegrity }

func (e *IntegrityError) Unwrap() []error {
	if e.err == nil {
		return []error{ErrEvidenceIntegrity}
	}
	return []error{ErrEvidenceIntegrity, e.err}
}

func integrityf(digest, reason, format string, args ...any) *IntegrityError {
	return &IntegrityError{Digest: digest, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// DefaultMediaType is applied when a caller does not declare one.
const DefaultMediaType = "application/octet-stream"

// File modes. Content is immutable once written, so it is stored read-only;
// the tree is private to the daemon's user, as the filesystem is the control
// plane's only access control (decisions.md D3).
const (
	dirMode     os.FileMode = 0o700
	contentMode os.FileMode = 0o400
)

// Meta is what a caller declares about content it is putting.
type Meta struct {
	// MediaType defaults to DefaultMediaType when empty.
	MediaType string
	// Class is the kind of evidence this is. It is required: unclassified
	// evidence cannot be reasoned about later.
	Class string
}

// Descriptor is what the store records about an artifact.
type Descriptor struct {
	Digest    string
	Size      int64
	MediaType string
	Class     string
	CreatedAt string
}

// Store is a content-addressed artifact store backed by a directory tree and
// the artifacts table.
type Store struct {
	root string
	db   *store.DB
	now  func() time.Time
}

// Open prepares the store rooted at root, creating it if necessary.
func Open(db *store.DB, root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("artifacts: resolve %q: %w", root, err)
	}
	if err := os.MkdirAll(filepath.Join(abs, "sha256"), dirMode); err != nil {
		return nil, fmt.Errorf("artifacts: create store root: %w", err)
	}
	return &Store{root: abs, db: db, now: time.Now}, nil
}

// OpenWithClock is Open with an injected clock, for deterministic timestamps.
func OpenWithClock(db *store.DB, root string, now func() time.Time) (*Store, error) {
	s, err := Open(db, root)
	if err != nil {
		return nil, err
	}
	s.now = now
	return s, nil
}

// Root is the absolute path of the store tree. It is for the daemon's own
// diagnostics; it never reaches a client (decisions.md D5).
func (s *Store) Root() string { return s.root }

// contentPath is the location of a digest's content. Callers outside this
// package never see it.
func (s *Store) contentPath(digest string) string {
	return filepath.Join(s.root, "sha256", digest[:2], digest)
}

// Put streams r into the store and records it, returning what the store now
// holds for that digest.
//
// Putting content that is already present is a no-op after the stored bytes
// are confirmed identical, and returns the existing record — including the
// metadata the first put declared.
func (s *Store) Put(ctx context.Context, r io.Reader, meta Meta) (Descriptor, error) {
	return s.put(ctx, r, "", meta)
}

// PutExpected is Put with the digest declared in advance. Content that does
// not hash to expected is refused and nothing is written: this is the path by
// which a worker's declared artifact digest is checked against the bytes it
// actually produced.
func (s *Store) PutExpected(ctx context.Context, r io.Reader, expected string, meta Meta) (Descriptor, error) {
	if err := canonical.CheckDigest(expected); err != nil {
		return Descriptor{}, fmt.Errorf("artifacts: expected digest: %w", err)
	}
	return s.put(ctx, r, expected, meta)
}

func (s *Store) put(ctx context.Context, r io.Reader, expected string, meta Meta) (Descriptor, error) {
	if meta.Class == "" {
		return Descriptor{}, errors.New("artifacts: meta.Class is required")
	}
	if meta.MediaType == "" {
		meta.MediaType = DefaultMediaType
	}

	tmp, err := os.CreateTemp(s.root, ".incoming-*")
	if err != nil {
		return Descriptor{}, fmt.Errorf("artifacts: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		_ = tmp.Close()
		return Descriptor{}, fmt.Errorf("artifacts: write content: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Descriptor{}, fmt.Errorf("artifacts: flush content: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Descriptor{}, fmt.Errorf("artifacts: close content: %w", err)
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	if expected != "" && digest != expected {
		return Descriptor{}, fmt.Errorf("%w: declared %s, computed %s",
			ErrDigestMismatch, expected, digest)
	}

	target := s.contentPath(digest)
	if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
		return Descriptor{}, fmt.Errorf("artifacts: create content directory: %w", err)
	}

	switch _, statErr := os.Stat(target); {
	case statErr == nil:
		// Already present. The incoming bytes hash to this digest by
		// construction, so any difference means the stored copy is damaged.
		same, err := sameContents(tmpName, target)
		if err != nil {
			return Descriptor{}, fmt.Errorf("artifacts: compare stored content: %w", err)
		}
		if !same {
			return Descriptor{}, integrityf(digest, "stored content differs from content with the same digest",
				"the copy at rest is damaged")
		}
	case errors.Is(statErr, os.ErrNotExist):
		if err := os.Chmod(tmpName, contentMode); err != nil {
			return Descriptor{}, fmt.Errorf("artifacts: set content mode: %w", err)
		}
		if err := os.Rename(tmpName, target); err != nil {
			return Descriptor{}, fmt.Errorf("artifacts: place content: %w", err)
		}
		committed = true
	default:
		return Descriptor{}, fmt.Errorf("artifacts: stat content: %w", statErr)
	}

	if err := s.db.InsertArtifact(ctx, store.InsertArtifactParams{
		Digest:    digest,
		Size:      size,
		MediaType: meta.MediaType,
		Class:     meta.Class,
		CreatedAt: s.now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return Descriptor{}, fmt.Errorf("artifacts: record %s: %w", digest, err)
	}

	return s.Stat(ctx, digest)
}

// Stat returns what the database records for a digest.
func (s *Store) Stat(ctx context.Context, digest string) (Descriptor, error) {
	if err := canonical.CheckDigest(digest); err != nil {
		return Descriptor{}, fmt.Errorf("artifacts: %w", err)
	}
	row, err := s.db.GetArtifact(ctx, digest)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Descriptor{}, fmt.Errorf("%w: %s", ErrNotFound, digest)
		}
		return Descriptor{}, err
	}
	return Descriptor{
		Digest:    row.Digest,
		Size:      row.Size,
		MediaType: row.MediaType,
		Class:     row.Class,
		CreatedAt: row.CreatedAt,
	}, nil
}

// Get returns a reader over an artifact's content, after confirming that the
// bytes on disk still hash to the digest and match the recorded size.
//
// A mismatch is an evidence-integrity failure and nothing is served. The
// verification pass completes before the returned handle is opened, so no
// caller ever receives a prefix of corrupt content.
//
// The store is only as trustworthy as the filesystem beneath it: content could
// in principle be replaced between the verification pass and the read. Files
// are written read-only and never rewritten in place, so that window requires
// an actor who could equally rewrite the database.
func (s *Store) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	desc, err := s.Stat(ctx, digest)
	if err != nil {
		return nil, err
	}

	path := s.contentPath(digest)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, integrityf(digest, "recorded artifact has no content on disk", "")
		}
		return nil, fmt.Errorf("artifacts: open content: %w", err)
	}

	sum, size, err := canonical.SHA256Reader(f)
	closeErr := f.Close()
	if err != nil {
		return nil, fmt.Errorf("artifacts: verify content: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("artifacts: verify content: %w", closeErr)
	}
	if sum != digest {
		return nil, integrityf(digest, "stored content does not hash to its digest",
			"recomputed %s", sum)
	}
	if size != desc.Size {
		return nil, integrityf(digest, "stored content size does not match the record",
			"on disk %d, recorded %d", size, desc.Size)
	}

	verified, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("artifacts: open verified content: %w", err)
	}
	return verified, nil
}

// sameContents reports whether two files hold identical bytes.
func sameContents(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer func() { _ = fa.Close() }()

	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer func() { _ = fb.Close() }()

	const chunk = 64 * 1024
	bufA := make([]byte, chunk)
	bufB := make([]byte, chunk)
	for {
		nA, errA := io.ReadFull(fa, bufA)
		nB, errB := io.ReadFull(fb, bufB)
		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return false, nil
		}
		endA := errors.Is(errA, io.EOF) || errors.Is(errA, io.ErrUnexpectedEOF)
		endB := errors.Is(errB, io.EOF) || errors.Is(errB, io.ErrUnexpectedEOF)
		if endA || endB {
			return endA == endB, nil
		}
		if errA != nil {
			return false, errA
		}
		if errB != nil {
			return false, errB
		}
	}
}
