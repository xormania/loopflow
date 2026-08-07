package canonical

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Errors reported by Marshal and Decode. They are sentinels so that callers —
// in particular the event chain, which must classify integrity failures — can
// test with errors.Is.
var (
	// ErrUnsupportedType is returned for any Go type outside the JSON value
	// model documented on the package.
	ErrUnsupportedType = errors.New("canonical: unsupported type")

	// ErrFloat is returned for floating-point values. Python and Go disagree
	// on their representation, so they are refused rather than guessed.
	ErrFloat = errors.New("canonical: floating-point numbers are not permitted")

	// ErrInvalidNumber is returned for a json.Number whose literal is not a
	// JSON integer.
	ErrInvalidNumber = errors.New("canonical: invalid number literal")

	// ErrInvalidUTF8 is returned for a string that is not valid UTF-8. This
	// includes any string carrying an encoded surrogate code point.
	ErrInvalidUTF8 = errors.New("canonical: invalid UTF-8")

	// ErrTypedNil is returned for a nil map or slice of a concrete type.
	// encoding/json writes null for these while an empty literal writes {} or
	// [], so the Go value alone does not say which document was meant. Write
	// an untyped nil for null, or an empty literal for an empty container.
	ErrTypedNil = errors.New("canonical: typed nil is ambiguous")
)

// Marshal returns the canonical JSON encoding of v.
//
// The result has no insignificant whitespace and no trailing newline: object
// members are separated by "," and keys from values by ":", object keys are
// sorted by Unicode code point, and every non-ASCII rune is escaped as \uXXXX.
//
// v must be drawn from the JSON value model described on the package. Values
// obtained from Decode always are.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeValue(&buf, v, "$"); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeValue(buf *bytes.Buffer, v any, path string) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case string:
		return encodeString(buf, x, path)
	case json.Number:
		return encodeNumber(buf, x, path)

	case int:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
		return nil
	case int8:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
		return nil
	case int16:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
		return nil
	case int32:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
		return nil
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
		return nil
	case uint:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
		return nil
	case uint8:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
		return nil
	case uint16:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
		return nil
	case uint32:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
		return nil
	case uint64:
		buf.WriteString(strconv.FormatUint(x, 10))
		return nil

	case float32, float64:
		return fmt.Errorf("%w: at %s", ErrFloat, path)

	case []any:
		if x == nil {
			return fmt.Errorf("%w: []any at %s", ErrTypedNil, path)
		}
		buf.WriteByte('[')
		for i, elem := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeValue(buf, elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil

	case map[string]any:
		if x == nil {
			return fmt.Errorf("%w: map[string]any at %s", ErrTypedNil, path)
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// UTF-8 byte order is identical to Unicode code-point order, so the
		// standard byte-wise string sort is exactly Python's sorted().
		sort.Strings(keys)

		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeString(buf, k, path+"."+k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := encodeValue(buf, x[k], path+"."+k); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil

	default:
		return encodeReflect(buf, v, path)
	}
}

// encodeReflect handles values whose dynamic type is not one of the literal
// cases above but whose shape is still in the JSON value model: named types
// such as events.Event, and containers of concrete element types.
//
// It is a widening of convenience, not of contract. Structs remain refused —
// there is no single obvious mapping from Go fields to the object Python would
// have hashed — as do floats, byte slices, and typed nils.
func encodeReflect(buf *bytes.Buffer, v any, path string) error {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		if rv.Bool() {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf.WriteString(strconv.FormatInt(rv.Int(), 10))
		return nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		buf.WriteString(strconv.FormatUint(rv.Uint(), 10))
		return nil

	case reflect.Float32, reflect.Float64:
		return fmt.Errorf("%w: at %s", ErrFloat, path)

	case reflect.String:
		return encodeString(buf, rv.String(), path)

	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return fmt.Errorf("%w: %s at %s", ErrTypedNil, rv.Type(), path)
		}
		return encodeValue(buf, rv.Elem().Interface(), path)

	case reflect.Map:
		if rv.IsNil() {
			return fmt.Errorf("%w: %s at %s", ErrTypedNil, rv.Type(), path)
		}
		if rv.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("%w: %s has non-string keys at %s",
				ErrUnsupportedType, rv.Type(), path)
		}
		keys := rv.MapKeys()
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.String()
		}
		sort.Strings(names)

		buf.WriteByte('{')
		for i, name := range names {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeString(buf, name, path+"."+name); err != nil {
				return err
			}
			buf.WriteByte(':')
			elem := rv.MapIndex(reflect.ValueOf(name).Convert(rv.Type().Key()))
			if err := encodeValue(buf, elem.Interface(), path+"."+name); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil

	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			// encoding/json base64-encodes byte slices while a generic array
			// walk would emit numbers. Neither is obviously right, so neither
			// is chosen.
			return fmt.Errorf("%w: %s is a byte container; encode it explicitly "+
				"as a string or an array of integers at %s",
				ErrUnsupportedType, rv.Type(), path)
		}
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return fmt.Errorf("%w: %s at %s", ErrTypedNil, rv.Type(), path)
		}
		buf.WriteByte('[')
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeValue(buf, rv.Index(i).Interface(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil

	default:
		return fmt.Errorf("%w: %T at %s (decode JSON with canonical.Decode, "+
			"not encoding/json.Unmarshal)", ErrUnsupportedType, v, path)
	}
}

// encodeNumber writes a json.Number, which must be a JSON integer literal.
//
// The literal is emitted verbatim so that integers too large for int64 — which
// Python represents exactly — survive a round trip. The single normalisation
// is -0, which Python parses to the int 0 and would re-emit as "0".
func encodeNumber(buf *bytes.Buffer, n json.Number, path string) error {
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		return fmt.Errorf("%w: %s at %s", ErrFloat, s, path)
	}
	if err := validateIntegerLiteral(s); err != nil {
		return fmt.Errorf("%w: %q at %s: %s", ErrInvalidNumber, s, path, err)
	}
	if s == "-0" {
		s = "0"
	}
	buf.WriteString(s)
	return nil
}

func validateIntegerLiteral(s string) error {
	if s == "" {
		return errors.New("empty")
	}
	i := 0
	if s[0] == '-' {
		i = 1
	}
	if i == len(s) {
		return errors.New("no digits")
	}
	if s[i] == '0' && len(s) > i+1 {
		return errors.New("leading zero")
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return errors.New("not a decimal integer")
		}
	}
	return nil
}

const hexDigits = "0123456789abcdef"

// encodeString writes s escaped exactly as Python's ensure_ascii=True form:
// the printable ASCII range 0x20-0x7E is literal apart from " and \, every
// other rune is escaped, and non-BMP runes become a surrogate pair. Hex
// digits are lowercase.
func encodeString(buf *bytes.Buffer, s string, path string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: at %s", ErrInvalidUTF8, path)
	}
	buf.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			buf.WriteString(`\"`)
		case r == '\\':
			buf.WriteString(`\\`)
		case r == '\b':
			buf.WriteString(`\b`)
		case r == '\f':
			buf.WriteString(`\f`)
		case r == '\n':
			buf.WriteString(`\n`)
		case r == '\r':
			buf.WriteString(`\r`)
		case r == '\t':
			buf.WriteString(`\t`)
		case r < 0x20:
			writeUnicodeEscape(buf, uint16(r))
		case r < 0x7f:
			// 0x20-0x7E literal. Note that / and & < > are NOT escaped:
			// Python does not escape them and Go's HTML escaping is bypassed
			// entirely by not using encoding/json to write.
			buf.WriteByte(byte(r))
		case r <= 0xffff:
			// Includes DEL (0x7F): ensure_ascii escapes every rune outside
			// 0x20-0x7E, so DEL is escaped here where Go would emit it raw.
			writeUnicodeEscape(buf, uint16(r))
		default:
			c := r - 0x10000
			writeUnicodeEscape(buf, uint16(0xd800+(c>>10)))
			writeUnicodeEscape(buf, uint16(0xdc00+(c&0x3ff)))
		}
	}
	buf.WriteByte('"')
	return nil
}

func writeUnicodeEscape(buf *bytes.Buffer, v uint16) {
	buf.WriteString(`\u`)
	buf.WriteByte(hexDigits[(v>>12)&0xf])
	buf.WriteByte(hexDigits[(v>>8)&0xf])
	buf.WriteByte(hexDigits[(v>>4)&0xf])
	buf.WriteByte(hexDigits[v&0xf])
}
