package artifacts

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xormania/loopflow/internal/canonical"
	"github.com/xormania/loopflow/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()

	db, err := store.Open(t.Context(), filepath.Join(root, "state", "control.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	s, err := Open(db, filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("open artifacts: %v", err)
	}
	return s
}

func mustPut(t *testing.T, s *Store, content string) Descriptor {
	t.Helper()
	desc, err := s.Put(t.Context(), strings.NewReader(content), Meta{
		MediaType: "text/plain",
		Class:     "worker-report",
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	return desc
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// Test item 5: put/get round-trip.
func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	const content = "RESULT: GREEN (14 cases)\n"

	desc := mustPut(t, s, content)
	if desc.Digest != canonical.SHA256Bytes([]byte(content)) {
		t.Errorf("digest = %s, want the SHA-256 of the content", desc.Digest)
	}
	if desc.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", desc.Size, len(content))
	}
	if desc.MediaType != "text/plain" || desc.Class != "worker-report" {
		t.Errorf("metadata not recorded: %+v", desc)
	}
	if desc.CreatedAt == "" {
		t.Error("created_at not recorded")
	}

	rc, err := s.Get(t.Context(), desc.Digest)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := readAll(t, rc); got != content {
		t.Errorf("content = %q, want %q", got, content)
	}

	stat, err := s.Stat(t.Context(), desc.Digest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat != desc {
		t.Errorf("stat = %+v, want %+v", stat, desc)
	}
}

func TestPutHandlesEmptyAndLargeContent(t *testing.T) {
	s := newTestStore(t)

	empty := mustPut(t, s, "")
	if empty.Size != 0 {
		t.Errorf("empty size = %d, want 0", empty.Size)
	}
	if got := readAll(t, mustGet(t, s, empty.Digest)); got != "" {
		t.Errorf("empty content = %q", got)
	}

	// Larger than the comparison chunk used when deduping, so the multi-chunk
	// path is exercised.
	large := strings.Repeat("abcdefgh", 40_000) // 320 KiB
	desc := mustPut(t, s, large)
	if desc.Size != int64(len(large)) {
		t.Errorf("size = %d, want %d", desc.Size, len(large))
	}
	if got := readAll(t, mustGet(t, s, desc.Digest)); got != large {
		t.Error("large content did not round-trip")
	}
}

// Test item 5: PutExpected mismatch refused.
func TestPutExpectedRefusesMismatchAndWritesNothing(t *testing.T) {
	s := newTestStore(t)
	const content = "declared one thing, produced another"
	wrong := canonical.SHA256Bytes([]byte("something else"))

	_, err := s.PutExpected(t.Context(), strings.NewReader(content), wrong, Meta{Class: "evidence"})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("error = %v, want ErrDigestMismatch", err)
	}

	// Neither digest is admitted: not the declared one, not the actual one.
	actual := canonical.SHA256Bytes([]byte(content))
	for _, digest := range []string{wrong, actual} {
		if _, err := s.Stat(t.Context(), digest); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%s) = %v, want ErrNotFound", digest[:8], err)
		}
		if _, err := os.Stat(s.contentPath(digest)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("content for %s was written despite the refusal", digest[:8])
		}
	}
	assertNoTempFiles(t, s)

	// A correctly declared digest is admitted.
	desc, err := s.PutExpected(t.Context(), strings.NewReader(content), actual, Meta{Class: "evidence"})
	if err != nil {
		t.Fatalf("PutExpected with the correct digest: %v", err)
	}
	if desc.Digest != actual {
		t.Errorf("digest = %s, want %s", desc.Digest, actual)
	}
}

func TestPutExpectedRejectsMalformedDigest(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []string{"", "abc", strings.ToUpper(canonical.SHA256Bytes(nil))} {
		if _, err := s.PutExpected(t.Context(), strings.NewReader("x"), bad, Meta{Class: "e"}); err == nil {
			t.Errorf("PutExpected(%q) succeeded, want an error", bad)
		}
	}
}

// Test item 5: on-disk corruption detected at Get.
func TestGetDetectsOnDiskCorruption(t *testing.T) {
	s := newTestStore(t)
	const content = "SECURITY GATE PASS"
	desc := mustPut(t, s, content)

	path := s.contentPath(desc.Digest)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(path, []byte("SECURITY GATE FAIL"), 0o600); err != nil {
		t.Fatalf("corrupt content: %v", err)
	}

	rc, err := s.Get(t.Context(), desc.Digest)
	if rc != nil {
		t.Error("Get returned a reader over corrupt content")
		_ = rc.Close()
	}
	var integrity *IntegrityError
	if !errors.As(err, &integrity) {
		t.Fatalf("error = %v, want an IntegrityError", err)
	}
	if !errors.Is(err, ErrEvidenceIntegrity) {
		t.Errorf("error %v does not match ErrEvidenceIntegrity", err)
	}
	if integrity.Classification() != ClassEvidenceIntegrity {
		t.Errorf("classification = %q, want %q", integrity.Classification(), ClassEvidenceIntegrity)
	}
	if integrity.Digest != desc.Digest {
		t.Errorf("integrity error names %s, want %s", integrity.Digest, desc.Digest)
	}
}

func TestGetDetectsMissingContent(t *testing.T) {
	s := newTestStore(t)
	desc := mustPut(t, s, "recorded then lost")

	if err := os.Remove(s.contentPath(desc.Digest)); err != nil {
		t.Fatalf("remove content: %v", err)
	}

	rc, err := s.Get(t.Context(), desc.Digest)
	if rc != nil {
		_ = rc.Close()
		t.Error("Get returned a reader for missing content")
	}
	if !errors.Is(err, ErrEvidenceIntegrity) {
		t.Errorf("error = %v, want evidence-integrity", err)
	}
}

func TestGetDetectsSizeDisagreement(t *testing.T) {
	s := newTestStore(t)
	desc := mustPut(t, s, "content")

	// The bytes still hash correctly; only the recorded size is wrong. That is
	// still a disagreement between the store and its index.
	if _, err := s.db.SQL().ExecContext(t.Context(),
		"UPDATE artifacts SET size = size + 1 WHERE digest = ?", desc.Digest); err != nil {
		t.Fatalf("tamper size: %v", err)
	}
	if _, err := s.Get(t.Context(), desc.Digest); !errors.Is(err, ErrEvidenceIntegrity) {
		t.Errorf("error = %v, want evidence-integrity", err)
	}
}

// Test item 5: duplicate put dedupes.
func TestDuplicatePutDedupes(t *testing.T) {
	s := newTestStore(t)
	const content = "identical bytes"

	first := mustPut(t, s, content)
	info, err := os.Stat(s.contentPath(first.Digest))
	if err != nil {
		t.Fatalf("stat content: %v", err)
	}

	// A second put of the same bytes, declaring different metadata.
	second, err := s.Put(t.Context(), strings.NewReader(content), Meta{
		MediaType: "application/json",
		Class:     "different-class",
	})
	if err != nil {
		t.Fatalf("second put: %v", err)
	}

	if second.Digest != first.Digest {
		t.Errorf("digest changed: %s then %s", first.Digest, second.Digest)
	}
	// The first record wins: content is immutable and so is what was said
	// about it when it was admitted.
	if second.MediaType != first.MediaType || second.Class != first.Class || second.CreatedAt != first.CreatedAt {
		t.Errorf("dedupe rewrote the record: %+v then %+v", first, second)
	}

	var count int
	if err := s.db.SQL().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM artifacts").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("artifacts table holds %d rows, want 1", count)
	}

	after, err := os.Stat(s.contentPath(first.Digest))
	if err != nil {
		t.Fatalf("stat content: %v", err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Error("dedupe rewrote the content file")
	}
	assertNoTempFiles(t, s)

	if got := readAll(t, mustGet(t, s, first.Digest)); got != content {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestDuplicatePutDetectsDamagedStoredCopy(t *testing.T) {
	s := newTestStore(t)
	const content = "original"
	desc := mustPut(t, s, content)

	// Damage the copy at rest, then put the same bytes again. The incoming
	// bytes hash to the digest by construction, so the difference can only be
	// in what is already stored.
	path := s.contentPath(desc.Digest)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
		t.Fatalf("damage content: %v", err)
	}

	_, err := s.Put(t.Context(), strings.NewReader(content), Meta{Class: "worker-report"})
	if !errors.Is(err, ErrEvidenceIntegrity) {
		t.Fatalf("error = %v, want evidence-integrity", err)
	}
	assertNoTempFiles(t, s)
}

func TestContentIsStoredPrivateAndReadOnly(t *testing.T) {
	s := newTestStore(t)
	desc := mustPut(t, s, "evidence")

	info, err := os.Stat(s.contentPath(desc.Digest))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != contentMode {
		t.Errorf("content mode = %04o, want %04o", perm, contentMode)
	}

	// The two-character shard directory and the store root are private.
	for _, dir := range []string{
		filepath.Dir(s.contentPath(desc.Digest)),
		filepath.Join(s.Root(), "sha256"),
		s.Root(),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != dirMode {
			t.Errorf("%s mode = %04o, want %04o", dir, perm, dirMode)
		}
	}

	// The digest is the identity: the path is derived from it, not stored.
	var stored int
	if err := s.db.SQL().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM artifacts WHERE digest LIKE '%/%'").Scan(&stored); err != nil {
		t.Fatalf("query: %v", err)
	}
	if stored != 0 {
		t.Error("a path-like value was stored as a digest")
	}
}

func TestPutRequiresAClass(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put(t.Context(), strings.NewReader("x"), Meta{}); err == nil {
		t.Error("put succeeded without a class")
	}
	// Media type has a documented default; class does not.
	desc, err := s.Put(t.Context(), strings.NewReader("x"), Meta{Class: "evidence"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if desc.MediaType != DefaultMediaType {
		t.Errorf("media type = %q, want %q", desc.MediaType, DefaultMediaType)
	}
}

func TestStatAndGetRejectUnknownAndMalformedDigests(t *testing.T) {
	s := newTestStore(t)

	unknown := canonical.SHA256Bytes([]byte("never stored"))
	if _, err := s.Stat(t.Context(), unknown); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(t.Context(), unknown); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrNotFound", err)
	}

	for _, bad := range []string{"", "zz", strings.Repeat("A", 64)} {
		if _, err := s.Stat(t.Context(), bad); err == nil {
			t.Errorf("Stat(%q) succeeded, want an error", bad)
		}
	}
}

// Content present on disk but absent from the index is not served: the
// database is the record of what was admitted.
func TestUnrecordedContentIsNotServed(t *testing.T) {
	s := newTestStore(t)
	desc := mustPut(t, s, "admitted then unrecorded")

	if _, err := s.db.SQL().ExecContext(t.Context(),
		"DELETE FROM artifacts WHERE digest = ?", desc.Digest); err != nil {
		t.Fatalf("delete record: %v", err)
	}
	if _, err := s.Get(t.Context(), desc.Digest); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
}

func mustGet(t *testing.T, s *Store, digest string) io.ReadCloser {
	t.Helper()
	rc, err := s.Get(t.Context(), digest)
	if err != nil {
		t.Fatalf("get %s: %v", digest, err)
	}
	return rc
}

func assertNoTempFiles(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(s.Root())
	if err != nil {
		t.Fatalf("read store root: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".incoming-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
