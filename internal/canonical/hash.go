package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// ZeroDigest is the 64-character all-zero digest used as the prev link of the
// first event in a chain (decisions.md D6).
const ZeroDigest = "0000000000000000000000000000000000000000000000000000000000000000"

// DigestLen is the length of a lowercase-hex SHA-256 digest.
const DigestLen = 64

// SHA256Hex returns the lowercase-hex SHA-256 of v's canonical JSON encoding.
func SHA256Hex(v any) (string, error) {
	b, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return SHA256Bytes(b), nil
}

// SHA256Bytes returns the lowercase-hex SHA-256 of raw bytes. It is the digest
// form used by the artifact store, where content is opaque and is hashed as it
// lies rather than canonicalised.
func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SHA256Reader streams r to completion and returns its lowercase-hex SHA-256
// along with the number of bytes read.
func SHA256Reader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ValidDigest reports whether s is a well-formed lowercase-hex SHA-256 digest.
// Uppercase is rejected: a digest is compared as a string throughout the
// control plane, so exactly one spelling may exist.
func ValidDigest(s string) bool {
	if len(s) != DigestLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CheckDigest returns an error describing why s is not a valid digest.
func CheckDigest(s string) error {
	if !ValidDigest(s) {
		return fmt.Errorf("canonical: %q is not a lowercase-hex SHA-256 digest", s)
	}
	return nil
}
