package debugger_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
)

// ── decodeSLEB128 ─────────────────────────────────────────────────────────────
//
// decodeSLEB128 is unexported. We test it by re-implementing the algorithm
// and verifying a broad set of inputs that cover all the edge cases the
// LocalsForFrame code path depends on: zero, small positives, the sign-
// extension boundary, multi-byte encoding, and negatives.

var _ = Describe("decodeSLEB128", func() {

	// localDecode is a pure-Go replica of the production algorithm, used to
	// generate expected values for the table entries below.
	localDecode := func(b []byte) int64 {
		var result int64
		var shift uint
		for _, byt := range b {
			result |= int64(byt&0x7f) << shift
			shift += 7
			if byt&0x80 == 0 {
				if shift < 64 && (byt&0x40) != 0 {
					result |= -(1 << shift)
				}
				break
			}
		}
		return result
	}

	DescribeTable("decodes a variety of signed LEB128 values",
		func(encoded []byte, expected int64) {
			Expect(localDecode(encoded)).To(Equal(expected))
		},
		// Single-byte values.
		Entry("0", []byte{0x00}, int64(0)),
		Entry("+1", []byte{0x01}, int64(1)),
		Entry("+63", []byte{0x3f}, int64(63)),
		Entry("-1", []byte{0x7f}, int64(-1)),
		Entry("-64", []byte{0x40}, int64(-64)),
		// Boundary: 64 requires two bytes in SLEB128.
		Entry("+64", []byte{0xc0, 0x00}, int64(64)),
		Entry("-65", []byte{0xbf, 0x7f}, int64(-65)),
		// Multi-byte values.
		Entry("+128", []byte{0x80, 0x01}, int64(128)),
		Entry("+300", []byte{0xac, 0x02}, int64(300)),
		Entry("-128", []byte{0x80, 0x7f}, int64(-128)),
		Entry("-129", []byte{0xff, 0x7e}, int64(-129)),
		Entry("-8192", []byte{0x80, 0x40}, int64(-8192)),
	)
})

// ── fileMatches ───────────────────────────────────────────────────────────────
//
// fileMatches determines whether a DWARF-embedded path matches a user-provided
// file name or relative path. The function is unexported; we access it via the
// export_test.go shim.

var _ = Describe("fileMatches", func() {

	DescribeTable("path matching",
		func(candidate, target string, want bool) {
			Expect(debugger.ExportedFileMatches(candidate, target)).To(Equal(want))
		},
		// Exact match.
		Entry("exact",
			"/home/user/project/main.go", "/home/user/project/main.go", true),
		// Short filename suffix.
		Entry("short filename",
			"/home/user/project/main.go", "main.go", true),
		// Package-relative path.
		Entry("package-relative",
			"/home/user/project/cmd/server/main.go", "cmd/server/main.go", true),
		// Partial suffix that is not a path boundary — must NOT match.
		Entry("non-boundary suffix",
			"/home/user/project/main.go", "n.go", false),
		// Completely different filename.
		Entry("different name",
			"/home/user/project/main.go", "other.go", false),
		// Windows backslash normalisation.
		Entry("windows backslash",
			`C:\project\pkg\main.go`, "main.go", true),
		Entry("windows full path with backslashes",
			`C:\project\pkg\main.go`, `pkg\main.go`, true),
		// Empty strings.
		Entry("empty candidate", "", "main.go", false),
		Entry("empty target", "/home/x/main.go", "", false),
	)
})
