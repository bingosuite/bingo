package protocol_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// The producer's per-element string limit is counted in Go, and enforced again
// in TypeScript by the binding consumer. Both sides already test the rule; what
// they could not test is that they agree on the SAME BYTES — and disagreement at
// the boundary is the whole hazard, because the producer would emit something it
// believes legal and the consumer would be obliged to reject it, deterministically,
// on every retry. See issue #194.
//
// So the boundary cases live in one checked-in fixture that both languages read.
// This spec is its generator and its drift gate: it rebuilds every case from
// first principles and fails if the file disagrees. The TypeScript suite reads
// the same file and asserts its decoder reaches the identical verdict on the
// identical wire strings.
const stringBoundaryFixture = "testdata/goroutine_string_boundary.json"

// stringBoundaryCase is one input measured on both sides of the wire.
type stringBoundaryCase struct {
	Name string `json:"name"`
	// InputBase64 is the raw producer-side string, base64-encoded so bytes that
	// are not valid UTF-8 survive being carried in a JSON fixture.
	InputBase64 string `json:"inputBase64"`
	// Wire is the same string as it appears after JSON encoding — the exact
	// value a consumer parses. It differs from the input only where encoding/json
	// substituted U+FFFD for an invalid byte.
	Wire string `json:"wire"`
	// UTF16Units is Wire's length in UTF-16 code units: JavaScript's String
	// .length, and what the producer's limit is denominated in.
	UTF16Units int `json:"utf16Units"`
	// Accepted is the producer's verdict. The consumer must reach the same one.
	Accepted bool `json:"accepted"`
}

type stringBoundaryFile struct {
	MaxGoroutineStringLength int                  `json:"maxGoroutineStringLength"`
	Cases                    []stringBoundaryCase `json:"cases"`
}

// buildStringBoundaryCases enumerates the inputs where a byte-, rune-, or
// UTF-16-denominated limit disagree, plus the invalid-UTF-8 forms whose length
// only settles after JSON encoding.
func buildStringBoundaryCases() []stringBoundaryCase {
	limit := protocol.MaxGoroutineStringLength
	ascii := func(n int) string { return strings.Repeat("a", n) }
	// U+1F600: one code point, TWO UTF-16 code units.
	astral := func(units int) string { return strings.Repeat("\U0001F600", units/2) }
	// U+4E00: one code point, ONE UTF-16 code unit, but THREE bytes — the case a
	// byte-denominated limit gets wrong in the opposite direction.
	bmp3 := func(units int) string { return strings.Repeat("\u4E00", units) }

	inputs := []struct {
		name string
		in   string
	}{
		{"ascii at the limit", ascii(limit)},
		{"ascii one unit over", ascii(limit + 1)},
		{"astral at the limit", astral(limit)},
		{"astral one unit over", astral(limit) + "a"},
		{"astral code points at the limit count double", strings.Repeat("\U0001F600", limit)},
		{"three-byte BMP at the limit", bmp3(limit)},
		{"three-byte BMP one unit over", bmp3(limit + 1)},
		// encoding/json substitutes one U+FFFD per invalid byte, so each bad byte
		// costs the consumer exactly one UTF-16 unit.
		{"invalid utf-8 at the limit", strings.Repeat("\xff", limit)},
		{"invalid utf-8 one unit over", strings.Repeat("\xff", limit+1)},
		// A UTF-16 surrogate encoded as UTF-8 is invalid: three bad bytes, so
		// three replacement characters, not one.
		{"lone surrogate bytes at the limit", strings.Repeat("\xed\xa0\x80", limit/3)},
		{"lone surrogate bytes over the limit", strings.Repeat("\xed\xa0\x80", limit/3+1)},
		// U+2028/U+2029 terminate a line in older JavaScript parsers; they must
		// survive as ordinary single-unit characters.
		{"line separators at the limit", strings.Repeat("\u2028\u2029", limit/2)},
		{"line separators one unit over", strings.Repeat("\u2028\u2029", limit/2) + "\u2028"},
	}

	out := make([]stringBoundaryCase, 0, len(inputs))
	for _, c := range inputs {
		out = append(out, stringBoundaryCase{
			Name:        c.name,
			InputBase64: base64.StdEncoding.EncodeToString([]byte(c.in)),
			Wire:        wireForm(c.in),
			UTF16Units:  utf16Units(wireForm(c.in)),
			Accepted:    producerAccepts(c.in),
		})
	}
	return out
}

// wireForm returns the string as it survives a JSON round trip, which is the
// only form a consumer ever sees.
func wireForm(s string) string {
	raw, err := json.Marshal(s)
	Expect(err).NotTo(HaveOccurred())
	var out string
	Expect(json.Unmarshal(raw, &out)).To(Succeed())
	return out
}

// producerAccepts reports the packer's verdict on a string. The element is
// deliberately NOT the current goroutine: a rejected anchor degrades the whole
// result, whereas a rejected ordinary element is skipped, which isolates the
// string rule from the anchor rule.
func producerAccepts(s string) bool {
	out, _ := protocol.PackGoroutines([]protocol.Goroutine{
		{ID: 1, Status: "running", Current: true},
		{ID: 2, Status: s},
	}, false)
	return len(out.Goroutines) == 2
}

var _ = Describe("cross-language string boundary fixture", func() {
	It("matches the checked-in fixture the TypeScript consumer reads", func() {
		want := stringBoundaryFile{
			MaxGoroutineStringLength: protocol.MaxGoroutineStringLength,
			Cases:                    buildStringBoundaryCases(),
		}
		encoded, err := json.MarshalIndent(want, "", "  ")
		Expect(err).NotTo(HaveOccurred())
		encoded = append(encoded, '\n')

		if os.Getenv("BINGO_UPDATE_FIXTURES") != "" {
			Expect(os.MkdirAll(filepath.Dir(stringBoundaryFixture), 0o750)).To(Succeed())
			Expect(os.WriteFile(stringBoundaryFixture, encoded, 0o600)).To(Succeed())
		}

		onDisk, err := os.ReadFile(stringBoundaryFixture)
		Expect(err).NotTo(HaveOccurred(),
			"regenerate with BINGO_UPDATE_FIXTURES=1 go test ./pkg/protocol/")
		Expect(string(onDisk)).To(Equal(string(encoded)),
			"the fixture the consumer enforces against has drifted from the producer; "+
				"regenerate with BINGO_UPDATE_FIXTURES=1 and re-run the VS Code suite")
	})

	It("covers both verdicts at the boundary", func() {
		// A fixture that only carried acceptances would let a consumer that
		// rejects everything pass, and vice versa.
		cases := buildStringBoundaryCases()
		var accepted, rejected int
		for _, c := range cases {
			if c.Accepted {
				accepted++
			} else {
				rejected++
			}
		}
		Expect(accepted).To(BeNumerically(">=", 5))
		Expect(rejected).To(BeNumerically(">=", 5))
	})

	It("states the consumer-visible length of every case", func() {
		for _, c := range buildStringBoundaryCases() {
			Expect(utf16.Encode([]rune(c.Wire))).To(HaveLen(c.UTF16Units), c.Name)
			// The verdict must be exactly the UTF-16 rule, measured on the form
			// the consumer receives — not on the producer's raw bytes.
			Expect(c.Accepted).To(Equal(c.UTF16Units <= protocol.MaxGoroutineStringLength), c.Name)
		}
	})
})
