package canonical

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

var (
	// ErrDuplicateKey is returned when an object contains the same key twice.
	// encoding/json silently keeps the last such member; this package refuses
	// the document, because its meaning would otherwise depend on the parser
	// that read it.
	ErrDuplicateKey = errors.New("canonical: duplicate object key")

	// ErrLoneSurrogate is returned for a \uD800-\uDFFF escape that is not part
	// of a valid pair. encoding/json silently substitutes U+FFFD, which
	// rehashes to a different digest than the bytes that were written.
	ErrLoneSurrogate = errors.New("canonical: lone surrogate escape")

	// ErrTrailingData is returned when input holds more than one JSON value.
	ErrTrailingData = errors.New("canonical: trailing data after JSON value")
)

// Decode parses JSON into the value model Marshal accepts.
//
// It differs from encoding/json.Unmarshal into an any in three ways, each of
// which exists to keep a decode/encode round trip byte-exact:
//
//   - numbers are kept as json.Number, preserving the exact literal instead of
//     collapsing to float64;
//   - duplicate object keys are an error, not last-one-wins; and
//   - lone surrogate escapes are an error, not U+FFFD.
func Decode(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: input is not UTF-8", ErrInvalidUTF8)
	}
	if err := checkSurrogateEscapes(data); err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	v, err := decodeValue(dec, "$")
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrTrailingData
	}
	return v, nil
}

// DecodeObject is Decode restricted to a JSON object, which is the shape of
// every event and every packet state.
func DecodeObject(data []byte) (map[string]any, error) {
	v, err := Decode(data)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("canonical: expected a JSON object, got %T", v)
	}
	return obj, nil
}

func decodeValue(dec *json.Decoder, path string) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("canonical: at %s: %w", path, err)
	}
	return decodeFromToken(dec, tok, path)
}

func decodeFromToken(dec *json.Decoder, tok json.Token, path string) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return decodeObjectBody(dec, path)
		case '[':
			return decodeArrayBody(dec, path)
		default:
			return nil, fmt.Errorf("canonical: unexpected %q at %s", t, path)
		}
	case string, bool, json.Number, nil:
		return t, nil
	default:
		return nil, fmt.Errorf("canonical: unexpected token %T at %s", tok, path)
	}
}

func decodeObjectBody(dec *json.Decoder, path string) (map[string]any, error) {
	obj := map[string]any{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("canonical: at %s: %w", path, err)
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			return obj, nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("canonical: non-string object key at %s", path)
		}
		if _, dup := obj[key]; dup {
			return nil, fmt.Errorf("%w: %q at %s", ErrDuplicateKey, key, path)
		}
		val, err := decodeValue(dec, path+"."+key)
		if err != nil {
			return nil, err
		}
		obj[key] = val
	}
}

func decodeArrayBody(dec *json.Decoder, path string) ([]any, error) {
	arr := []any{}
	for i := 0; ; i++ {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("canonical: at %s: %w", path, err)
		}
		if d, ok := tok.(json.Delim); ok && d == ']' {
			return arr, nil
		}
		val, err := decodeFromToken(dec, tok, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
}

// checkSurrogateEscapes scans the raw bytes for \uXXXX escapes inside string
// literals and requires every surrogate to be a correctly ordered pair.
//
// This has to run on the raw input: by the time encoding/json hands back a
// decoded string, a lone surrogate has already become U+FFFD and is
// indistinguishable from a genuine replacement character.
func checkSurrogateEscapes(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(data) {
				return nil // malformed input; the decoder reports it
			}
			if data[i+1] != 'u' {
				i++ // skip the escaped character
				continue
			}
			hi, ok := parseHex4(data, i+2)
			if !ok {
				return nil // malformed escape; the decoder reports it
			}
			switch {
			case hi >= 0xdc00 && hi <= 0xdfff:
				return fmt.Errorf("%w: \\u%04x has no preceding high surrogate", ErrLoneSurrogate, hi)
			case hi >= 0xd800 && hi <= 0xdbff:
				lo, ok := parseHex4(data, i+8)
				if !ok || i+6 >= len(data) || data[i+6] != '\\' || data[i+7] != 'u' ||
					lo < 0xdc00 || lo > 0xdfff {
					return fmt.Errorf("%w: \\u%04x is not followed by a low surrogate", ErrLoneSurrogate, hi)
				}
				i += 11 // consume both escapes
				continue
			}
			i += 5 // consume this escape
		}
	}
	return nil
}

func parseHex4(data []byte, off int) (uint32, bool) {
	if off+4 > len(data) {
		return 0, false
	}
	var v uint32
	for _, c := range data[off : off+4] {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint32(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint32(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | uint32(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}
