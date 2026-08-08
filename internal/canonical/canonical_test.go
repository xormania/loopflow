package canonical_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xormania/loopflow/internal/canonical"
)

// Expected values below are written with doubled backslashes in interpreted
// string literals: "\\u00e9" is the six-character escape text é, not the
// character é. Getting that backwards is the exact bug these tests exist to
// catch, so the two are never spelled the same way here.

// goldenVectorEvent is a real native event observed in production and recorded
// in environment-facts.md 3c / phase-1-foundation.md. It is reproduced here
// verbatim. If the test below fails, the encoder is wrong; the vector is never
// edited to make it pass.
const goldenVectorEvent = `{
  "hash": "8d828243335339b5dbd0ba4c5929ca61251c38d688379a108c7e283b922087ae",
  "issue_key": "initialized",
  "outcome": "passed",
  "phase": "init",
  "prev": "0000000000000000000000000000000000000000000000000000000000000000",
  "seq": 1,
  "state_sha256": "0e251dcac955cf58eede51cfd535ea5419c31dc811f5b93b1d24f6588d63886e",
  "time": "2026-08-05T20:32:30.615324Z"
}`

// Test item 1 of the Phase 1 acceptance floor.
func TestGoldenVector(t *testing.T) {
	event, err := canonical.DecodeObject([]byte(goldenVectorEvent))
	if err != nil {
		t.Fatalf("decode golden vector: %v", err)
	}

	recorded, ok := event["hash"].(string)
	if !ok {
		t.Fatalf("golden vector hash is %T, want string", event["hash"])
	}
	delete(event, "hash")

	got, err := canonical.SHA256Hex(event)
	if err != nil {
		t.Fatalf("hash golden vector: %v", err)
	}
	if got != recorded {
		encoded, _ := canonical.Marshal(event)
		t.Fatalf("golden vector hash mismatch:\n got %s\nwant %s\ncanonical bytes: %s",
			got, recorded, encoded)
	}
}

// TestGoldenVectorCanonicalBytes pins the exact byte form the hash is taken
// over, so a change that alters the encoding fails with a readable diff rather
// than only an opaque digest mismatch.
func TestGoldenVectorCanonicalBytes(t *testing.T) {
	const want = `{"issue_key":"initialized","outcome":"passed","phase":"init",` +
		`"prev":"0000000000000000000000000000000000000000000000000000000000000000",` +
		`"seq":1,` +
		`"state_sha256":"0e251dcac955cf58eede51cfd535ea5419c31dc811f5b93b1d24f6588d63886e",` +
		`"time":"2026-08-05T20:32:30.615324Z"}`

	event, err := canonical.DecodeObject([]byte(goldenVectorEvent))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	delete(event, "hash")

	got, err := canonical.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("canonical bytes mismatch:\n got %s\nwant %s", got, want)
	}
}

type pythonFixture struct {
	Generator string `json:"generator"`
	Form      string `json:"form"`
	Python    string `json:"python"`
	Cases     []struct {
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		Canonical string          `json:"canonical"`
		SHA256    string          `json:"sha256"`
	} `json:"cases"`
}

// Test item 6: cross-check against output recorded from a real CPython
// interpreter rather than against an assumption about what it produces.
func TestPythonFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "python-canonical.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx pythonFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	t.Logf("fixture from CPython %s: %s", fx.Python, fx.Form)

	for _, tc := range fx.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			v, err := canonical.Decode(tc.Input)
			if err != nil {
				t.Fatalf("decode input: %v", err)
			}
			got, err := canonical.Marshal(v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.Canonical {
				t.Errorf("canonical form differs from CPython:\n go     %s\n python %s",
					got, tc.Canonical)
			}
			gotSum, err := canonical.SHA256Hex(v)
			if err != nil {
				t.Fatalf("hash: %v", err)
			}
			if gotSum != tc.SHA256 {
				t.Errorf("sha256 mismatch: got %s, want %s", gotSum, tc.SHA256)
			}
		})
	}
}

// Test item 6: key ordering is by Unicode code point, which for valid UTF-8 is
// the same as byte order.
func TestKeyOrdering(t *testing.T) {
	obj := map[string]any{
		"b": 1, "a": 1, "A": 1, "B": 1, "0": 1, "_": 1, "~": 1,
		"é": 1, "中": 1, "😀": 1, "": 1, "ab": 1,
	}
	// Code-point order:
	// "" < "0" < "A" < "B" < "_" < "a" < "ab" < "b" < "~"
	//    < U+00E9 < U+4E2D < U+1F600
	want := `{"":1,"0":1,"A":1,"B":1,"_":1,"a":1,"ab":1,"b":1,"~":1,` +
		"\"\\u00e9\":1," +
		"\"\\u4e2d\":1," +
		"\"\\ud83d\\ude00\":1}"

	got, err := canonical.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("key order:\n got %s\nwant %s", got, want)
	}
}

// Test item 6: non-ASCII is escaped as \uXXXX, non-BMP as a surrogate pair,
// and hex digits are lowercase.
func TestNonASCIIEscaping(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"latin-1 supplement", "café", "\"caf\\u00e9\""},
		{"cjk", "中文", "\"\\u4e2d\\u6587\""},
		{"emoji surrogate pair", "😀", "\"\\ud83d\\ude00\""},
		{"clef surrogate pair", "𝄞", "\"\\ud834\\udd1e\""},
		{"nul", string(rune(0x00)), "\"\\u0000\""},
		{"unit separator", string(rune(0x1f)), "\"\\u001f\""},
		// DEL is outside Python's literal range 0x20-0x7E but inside the range
		// Go's encoder emits raw, so it is the sharpest single-byte case.
		{"del", string(rune(0x7f)), "\"\\u007f\""},
		{"nbsp", string(rune(0xa0)), "\"\\u00a0\""},
		{"short escapes", "\n\t\r\b\f", `"\n\t\r\b\f"`},
		{"quote and backslash", `a"b\c`, `"a\"b\\c"`},
		{"printable ascii stays literal", "plain ASCII ~!@#$%^*()_+", `"plain ASCII ~!@#$%^*()_+"`},
		{"boundary 0x20 and 0x7e", " ~", `" ~"`},
		{"mixed", "a" + string(rune(0x00)) + "b" + string(rune(0x7f)) + "c😀",
			"\"a\\u0000b\\u007fc\\ud83d\\ude00\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonical.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal(%q): %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal(%q):\n got %s\nwant %s", tc.in, got, tc.want)
			}
		})
	}
}

// Test item 6: Go's default HTML escaping of < > & must be disabled.
func TestHTMLEscapingDisabled(t *testing.T) {
	in := `<script>&"</script>`
	want := `"<script>&\"</script>"`

	got, err := canonical.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("HTML escaping not disabled:\n got %s\nwant %s", got, want)
	}
	// The same string through encoding/json is what we must NOT produce.
	std, _ := json.Marshal(in)
	if string(std) == string(got) {
		t.Fatal("test is vacuous: encoding/json already produces the canonical form")
	}
	// Forward slash is not escaped by Python and must not be by us.
	got, err = canonical.Marshal("a/b")
	if err != nil || string(got) != `"a/b"` {
		t.Errorf(`Marshal("a/b") = %s, %v; want "a/b"`, got, err)
	}
}

// Test item 6: integers render without exponent or decimal point, exactly,
// including values beyond float64's integer range.
func TestIntegerRendering(t *testing.T) {
	cases := []struct{ in, want string }{
		{`0`, `0`},
		{`-0`, `0`}, // Python parses -0 to the int 0 and re-emits "0"
		{`1`, `1`},
		{`-1`, `-1`},
		{`9223372036854775807`, `9223372036854775807`},
		{`-9223372036854775808`, `-9223372036854775808`},
		{`9007199254740993`, `9007199254740993`}, // 2^53+1: not exact as float64
		{`123456789012345678901234567890`, `123456789012345678901234567890`},
	}
	for _, tc := range cases {
		v, err := canonical.Decode([]byte(tc.in))
		if err != nil {
			t.Errorf("Decode(%s): %v", tc.in, err)
			continue
		}
		got, err := canonical.Marshal(v)
		if err != nil {
			t.Errorf("Marshal(%s): %v", tc.in, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("Marshal(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}

	// Go's native integer kinds render the same way.
	for _, v := range []any{int(-1), int8(-1), int16(-1), int32(-1), int64(-1)} {
		got, err := canonical.Marshal(v)
		if err != nil || string(got) != "-1" {
			t.Errorf("Marshal(%T(-1)) = %s, %v; want -1", v, got, err)
		}
	}
	for _, v := range []any{uint(7), uint8(7), uint16(7), uint32(7), uint64(7)} {
		got, err := canonical.Marshal(v)
		if err != nil || string(got) != "7" {
			t.Errorf("Marshal(%T(7)) = %s, %v; want 7", v, got, err)
		}
	}
}

func TestFloatsRejected(t *testing.T) {
	for _, v := range []any{float64(1), float64(1.5), float32(2.5)} {
		if _, err := canonical.Marshal(v); !errors.Is(err, canonical.ErrFloat) {
			t.Errorf("Marshal(%T) error = %v, want ErrFloat", v, err)
		}
	}
	// Fractional and exponent literals are refused rather than reformatted.
	for _, lit := range []string{`1.5`, `1e2`, `1E2`, `1.0`, `-2.5e-3`} {
		v, err := canonical.Decode([]byte(lit))
		if err != nil {
			t.Errorf("Decode(%s): %v", lit, err)
			continue
		}
		if _, err := canonical.Marshal(v); !errors.Is(err, canonical.ErrFloat) {
			t.Errorf("Marshal(%s) error = %v, want ErrFloat", lit, err)
		}
	}
	// The trap this guards: encoding/json turns every number into a float64,
	// so a value obtained that way must not silently hash.
	var viaStdlib any
	if err := json.Unmarshal([]byte(`{"seq":1}`), &viaStdlib); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := canonical.Marshal(viaStdlib); !errors.Is(err, canonical.ErrFloat) {
		t.Errorf("encoding/json-decoded value error = %v, want ErrFloat", err)
	}
}

func TestUnsupportedTypesRejected(t *testing.T) {
	type s struct{ A int }
	type byteArray [2]byte
	cases := []any{
		s{1},                   // struct: no obvious field-to-key mapping
		&s{1},                  // pointer to struct: same, after dereference
		[]byte("x"),            // base64 or array of numbers?
		byteArray{1, 2},        // same question, fixed size
		map[int]string{1: "a"}, // non-string keys have no JSON spelling
		make(chan int),         // no JSON meaning at all
		func() {},              // likewise
	}
	for _, v := range cases {
		if _, err := canonical.Marshal(v); !errors.Is(err, canonical.ErrUnsupportedType) {
			t.Errorf("Marshal(%T) error = %v, want ErrUnsupportedType", v, err)
		}
	}
}

func TestTypedNilsRejected(t *testing.T) {
	type named map[string]any
	var (
		nilMap    map[string]any
		nilSlice  []any
		nilNamed  named
		nilStrMap map[string]string
		nilPtr    *int
	)
	for _, v := range []any{nilMap, nilSlice, nilNamed, nilStrMap, nilPtr} {
		if _, err := canonical.Marshal(v); !errors.Is(err, canonical.ErrTypedNil) {
			t.Errorf("Marshal(%T nil) error = %v, want ErrTypedNil", v, err)
		}
	}
	// An untyped nil is unambiguous: it is null.
	got, err := canonical.Marshal(nil)
	if err != nil || string(got) != "null" {
		t.Errorf("Marshal(nil) = %s, %v; want null", got, err)
	}
	// Empty literals are unambiguous too.
	got, err = canonical.Marshal(map[string]any{})
	if err != nil || string(got) != "{}" {
		t.Errorf("Marshal(empty map) = %s, %v; want {}", got, err)
	}
	got, err = canonical.Marshal([]any{})
	if err != nil || string(got) != "[]" {
		t.Errorf("Marshal(empty slice) = %s, %v; want []", got, err)
	}
}

// Named types over the value model — the shape internal/events uses for its
// Event — encode identically to their underlying type. A Go type switch does
// not match a named type to its underlying type, so this needs its own path
// and its own test.
func TestNamedTypesAndConcreteContainers(t *testing.T) {
	type namedMap map[string]any
	type namedSlice []any
	type namedString string
	type namedInt int

	cases := []struct {
		name string
		in   any
		want string
	}{
		{"named map", namedMap{"b": 1, "a": "x"}, `{"a":"x","b":1}`},
		{"named slice", namedSlice{1, "a", true, nil}, `[1,"a",true,null]`},
		{"named string", namedString("é"), "\"\\u00e9\""},
		{"named int", namedInt(-7), `-7`},
		{"map of string", map[string]string{"b": "2", "a": "1"}, `{"a":"1","b":"2"}`},
		{"map of int", map[string]int{"b": 2, "a": 1}, `{"a":1,"b":2}`},
		{"slice of string", []string{"b", "a"}, `["b","a"]`},
		{"slice of int", []int{3, 2, 1}, `[3,2,1]`},
		{"array of int", [3]int{1, 2, 3}, `[1,2,3]`},
		{"nested named", namedMap{"inner": namedSlice{namedMap{"k": namedString("v")}}},
			`{"inner":[{"k":"v"}]}`},
		{"pointer is dereferenced", ptr(42), `42`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonical.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal(%T): %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal(%T) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}

	// The widening does not weaken the float refusal.
	type namedFloat float64
	if _, err := canonical.Marshal(namedFloat(1)); !errors.Is(err, canonical.ErrFloat) {
		t.Errorf("Marshal(namedFloat) error = %v, want ErrFloat", err)
	}
	if _, err := canonical.Marshal(map[string]float64{"a": 1}); !errors.Is(err, canonical.ErrFloat) {
		t.Errorf("Marshal(map[string]float64) error = %v, want ErrFloat", err)
	}
	if _, err := canonical.Marshal([]any{1, 2.5}); !errors.Is(err, canonical.ErrFloat) {
		t.Errorf("Marshal([]any with a float) error = %v, want ErrFloat", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestInvalidNumberLiteralsRejected(t *testing.T) {
	for _, lit := range []string{"", "-", "+1", "01", "-01", "1x", "0x10"} {
		if _, err := canonical.Marshal(json.Number(lit)); !errors.Is(err, canonical.ErrInvalidNumber) {
			t.Errorf("Marshal(json.Number(%q)) error = %v, want ErrInvalidNumber", lit, err)
		}
	}
}

func TestDuplicateKeysRejected(t *testing.T) {
	const in = `{"a":1,"b":2,"a":3}`
	if _, err := canonical.Decode([]byte(in)); !errors.Is(err, canonical.ErrDuplicateKey) {
		t.Errorf("Decode(%s) error = %v, want ErrDuplicateKey", in, err)
	}
	const nested = `{"outer":{"a":1,"a":2}}`
	if _, err := canonical.Decode([]byte(nested)); !errors.Is(err, canonical.ErrDuplicateKey) {
		t.Errorf("Decode(%s) error = %v, want ErrDuplicateKey", nested, err)
	}
	// Contrast: the standard library silently keeps the last member.
	var std map[string]any
	if err := json.Unmarshal([]byte(in), &std); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(std) != 2 {
		t.Fatal("test is vacuous: encoding/json no longer accepts duplicate keys")
	}
}

func TestLoneSurrogatesRejected(t *testing.T) {
	q := func(s string) string { return `"` + s + `"` }
	bad := []struct {
		name string
		in   string
	}{
		{"high alone", q("\\ud834")},
		{"low alone", q("\\udd1e")},
		{"high then non-surrogate", q("\\ud834A")},
		{"reversed pair", q("\\udd1e\\ud834")},
		{"high then plain escape", q(`\ud834\n`)},
		{"in object value", `{"k":` + q("\\ud800") + `}`},
		{"in object key", `{` + q("\\ud800") + `:"v"}`},
		{"in array", `["ok",` + q("\\udfff") + `]`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := canonical.Decode([]byte(tc.in)); !errors.Is(err, canonical.ErrLoneSurrogate) {
				t.Errorf("Decode(%s) error = %v, want ErrLoneSurrogate", tc.in, err)
			}
		})
	}

	// A well-formed pair is accepted and round-trips to the same escape text.
	good := q("\\ud834\\udd1e")
	v, err := canonical.Decode([]byte(good))
	if err != nil {
		t.Fatalf("Decode(%s): %v", good, err)
	}
	got, err := canonical.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != good {
		t.Errorf("surrogate pair round trip: got %s, want %s", got, good)
	}
	if v != "𝄞" {
		t.Errorf("decoded %q, want the musical clef rune", v)
	}

	// An escaped backslash immediately before surrogate-looking text is data,
	// not an escape introducer, and must be accepted.
	escapedBackslash := q(`\\ud834`)
	if _, err := canonical.Decode([]byte(escapedBackslash)); err != nil {
		t.Errorf("Decode(%s) = %v, want nil", escapedBackslash, err)
	}

	// Marshal refuses invalid UTF-8, which is how an encoded surrogate would
	// have to arrive on the Go side.
	if _, err := canonical.Marshal(string([]byte{0xed, 0xa0, 0x80})); !errors.Is(err, canonical.ErrInvalidUTF8) {
		t.Error("Marshal did not reject a WTF-8 encoded surrogate")
	}
	if _, err := canonical.Decode([]byte{'"', 0xff, '"'}); !errors.Is(err, canonical.ErrInvalidUTF8) {
		t.Error("Decode did not reject invalid UTF-8 input")
	}
}

func TestTrailingDataRejected(t *testing.T) {
	for _, in := range []string{`{} {}`, `1 2`, `[] null`} {
		if _, err := canonical.Decode([]byte(in)); !errors.Is(err, canonical.ErrTrailingData) {
			t.Errorf("Decode(%s) error = %v, want ErrTrailingData", in, err)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	const in = `{"b":[1,{"y":2,"x":[3,[],{}]},null],"a":{"k":"v"},"t":true,"z":"é"}`
	v, err := canonical.Decode([]byte(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	first, err := canonical.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again, err := canonical.Decode(first)
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	second, err := canonical.Marshal(again)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("not idempotent:\n first  %s\n second %s", first, second)
	}
	if !reflect.DeepEqual(v, again) {
		t.Error("decoded values differ after a canonical round trip")
	}
}

func TestDecodeObjectRejectsNonObjects(t *testing.T) {
	for _, in := range []string{`[]`, `1`, `"s"`, `null`, `true`} {
		if _, err := canonical.DecodeObject([]byte(in)); err == nil {
			t.Errorf("DecodeObject(%s) succeeded, want error", in)
		}
	}
}

func TestDigestHelpers(t *testing.T) {
	// The SHA-256 of the empty input is a fixed, widely published value.
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := canonical.SHA256Bytes(nil); got != emptySHA256 {
		t.Errorf("SHA256Bytes(nil) = %s, want %s", got, emptySHA256)
	}
	got, n, err := canonical.SHA256Reader(strings.NewReader(""))
	if err != nil || n != 0 || got != emptySHA256 {
		t.Errorf("SHA256Reader(empty) = %s, %d, %v", got, n, err)
	}
	sum, n, err := canonical.SHA256Reader(strings.NewReader("abc"))
	if err != nil || n != 3 {
		t.Fatalf("SHA256Reader: %s, %d, %v", sum, n, err)
	}
	if sum != canonical.SHA256Bytes([]byte("abc")) {
		t.Error("SHA256Reader and SHA256Bytes disagree")
	}

	if !canonical.ValidDigest(emptySHA256) {
		t.Error("ValidDigest rejected a valid digest")
	}
	for _, bad := range []string{
		"",
		strings.ToUpper(emptySHA256), // uppercase is a second spelling
		emptySHA256[:63],
		emptySHA256 + "0",
		strings.Repeat("g", 64),
	} {
		if canonical.ValidDigest(bad) {
			t.Errorf("ValidDigest(%q) = true, want false", bad)
		}
		if canonical.CheckDigest(bad) == nil {
			t.Errorf("CheckDigest(%q) = nil, want error", bad)
		}
	}
	if len(canonical.ZeroDigest) != canonical.DigestLen || !canonical.ValidDigest(canonical.ZeroDigest) {
		t.Error("ZeroDigest is not a well-formed digest")
	}
}
