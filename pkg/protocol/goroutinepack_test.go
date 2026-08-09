package protocol_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// maxSafeGoid is JavaScript's Number.MAX_SAFE_INTEGER. Goids above it cannot be
// represented exactly by the consumer, so a realistic worst case stops here.
const maxSafeGoid = 1<<53 - 1

// expectReportedSize checks the payload against the contract and, when the
// packer measured (rather than proving the fit by bound), that the size it
// reported is the real one. A wrong non-zero size still fails.
func expectReportedSize(
	kind protocol.EventKind,
	payload any,
	report protocol.GoroutinePackReport,
) int {
	GinkgoHelper()
	actual := eventBytes(kind, payload)
	Expect(actual).To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
	if report.Bytes != 0 {
		Expect(report.Bytes).To(Equal(actual))
	}
	return actual
}

// eventBytes measures a payload exactly the way the packer's contract is
// stated: a real Event at the widest sequence number the hub can stamp.
func eventBytes(kind protocol.EventKind, payload any) int {
	raw, err := protocol.MarshalEvent(protocol.MustEvent(kind, math.MaxUint64, payload))
	Expect(err).NotTo(HaveOccurred())
	return len(raw)
}

// packGoroutine builds a realistically-sized goroutine: three Locations with
// long Go-style paths and symbol names, which is what actually drives snapshot
// size in the field.
func packGoroutine(id, parent int) protocol.Goroutine {
	file := "/home/runner/go/src/github.com/bingosuite/bingo/internal/service/handler.go"
	return protocol.Goroutine{
		ID:         id,
		ParentID:   parent,
		Status:     "waiting",
		WaitReason: "chan receive",
		CurrentLoc: protocol.Location{File: file, Line: 1234, Function: "github.com/bingosuite/bingo/internal/service.(*Handler).Serve.func1"},
		StartLoc:   protocol.Location{File: file, Line: 88, Function: "github.com/bingosuite/bingo/internal/service.(*Handler).Serve.gowrap1"},
		CreatedLoc: protocol.Location{File: file, Line: 91, Function: "github.com/bingosuite/bingo/internal/service.(*Handler).Serve"},
		ThreadID:   id % 16,
	}
}

func packGoroutines(n int) []protocol.Goroutine {
	out := make([]protocol.Goroutine, 0, n)
	for i := 1; i <= n; i++ {
		parent := 0
		if i > 1 {
			parent = i / 2
		}
		out = append(out, packGoroutine(i, parent))
	}
	return out
}

func packThreads(n int) []protocol.Thread {
	out := make([]protocol.Thread, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, protocol.Thread{
			ID:         5000 + i,
			MID:        i,
			GoID:       i + 1,
			CurrentLoc: protocol.Location{File: "/home/runner/go/src/github.com/bingosuite/bingo/internal/service/handler.go", Line: 12, Function: "runtime.mcall"},
		})
	}
	return out
}

func goroutineIDs(gs []protocol.Goroutine) []int {
	out := make([]int, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.ID)
	}
	return out
}

func threadMIDs(ts []protocol.Thread) []int {
	out := make([]int, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.MID)
	}
	return out
}

var _ = Describe("goroutine event packing", func() {
	Describe("byte budget", func() {
		It("keeps an oversized snapshot at or under the cap, measured at the widest seq", func() {
			snap := protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192),
				Threads:    packThreads(64),
				Current:    4096,
			}
			out, report := protocol.PackSnapshot(snap, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).To(HaveLen(report.Goroutines))
			Expect(len(out.Goroutines)).To(BeNumerically("<", 8192), "the input must actually be too big")
			size := eventBytes(protocol.EventGoroutineSnapshot, out)
			Expect(size).To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
			Expect(report.Bytes).To(Equal(size), "the report must state the measured size")
		})

		It("keeps an oversized goroutine list at or under the cap", func() {
			out, report := protocol.PackGoroutines(packGoroutines(8192), false)

			Expect(report.Degraded).To(BeFalse())
			Expect(len(out.Goroutines)).To(BeNumerically("<", 8192))
			size := eventBytes(protocol.EventGoroutines, out)
			Expect(size).To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
			Expect(report.Bytes).To(Equal(size))
		})

		It("fills the budget rather than truncating early", func() {
			out, _ := protocol.PackGoroutines(packGoroutines(8192), false)
			size := eventBytes(protocol.EventGoroutines, out)
			// One more average element must not have fit: proof the packer used
			// the budget instead of stopping at a convenient round number.
			average := size / len(out.Goroutines)
			Expect(size + average).To(BeNumerically(">", protocol.MaxGoroutineEventBytes))
		})

		It("derives the reserve from the real schema, not a constant", func() {
			// Lifecycle deltas are reserved before any element is packed, so a
			// snapshot carrying large deltas must pack strictly fewer elements
			// than the same snapshot without them.
			created := make([]int, 4000)
			for i := range created {
				created[i] = math.MaxInt64 - i
			}
			bare, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192), Current: 1,
			}, false, false)
			loaded, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192), Current: 1, Created: created,
			}, false, false)

			Expect(len(loaded.Goroutines)).To(BeNumerically("<", len(bare.Goroutines)))
			Expect(eventBytes(protocol.EventGoroutineSnapshot, loaded)).
				To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
		})

		It("uses the same accounting for both payload shapes", func() {
			gs := packGoroutines(8192)
			snap, snapReport := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Current: 1,
			}, false, false)
			list, listReport := protocol.PackGoroutines(gs, false)

			Expect(snapReport.Bytes).To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
			Expect(listReport.Bytes).To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
			// The shapes differ only by a fixed envelope, so identical
			// per-element accounting lands their counts within a couple of
			// elements of each other. They need not agree on the exact boundary
			// element: skip-and-continue means a slightly smaller later element
			// can take the place of one that just missed.
			Expect(len(list.Goroutines) - len(snap.Goroutines)).To(BeNumerically("<=", 2))
			Expect(len(snap.Goroutines) - len(list.Goroutines)).To(BeNumerically("<=", 2))

			// Where the whole set fits, the shared ordering is directly
			// comparable and must be identical.
			small := packGoroutines(200)
			smallSnap, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: small, Current: 1,
			}, false, false)
			smallList, _ := protocol.PackGoroutines(small, false)
			Expect(goroutineIDs(smallSnap.Goroutines)).To(Equal(goroutineIDs(smallList.Goroutines)))
		})
	})

	Describe("JSON escaping additivity", func() {
		// The packer charges each element its standalone marshal length. That is
		// only exact if escaping is context-free, so every escape class the wire
		// can carry is checked against a real measurement.
		DescribeTable("stays exact for",
			func(text string) {
				gs := make([]protocol.Goroutine, 0, 64)
				for i := 1; i <= 64; i++ {
					g := packGoroutine(i, 0)
					g.CurrentLoc.File = text
					g.WaitReason = text
					gs = append(gs, g)
				}
				out, report := protocol.PackGoroutines(gs, false)

				Expect(out.Goroutines).To(HaveLen(64))
				expectReportedSize(protocol.EventGoroutines, out, report)
			},
			Entry("plain ascii", "internal/service/handler.go"),
			Entry("html-escaped runes", "chan<-recv & send > done <T>"),
			Entry("quotes and backslashes", `"quoted"\path\to\file"`),
			Entry("control characters", "line\u0001\u001f\tbreak\nreturn\r"),
			Entry("multibyte", "服务/处理器/goroutine·waiting — café"),
			Entry("surrogate pair", "worker \U0001F600 spawn \U0001F680 pool"),
			Entry("line separators", "before\u2028middle\u2029after"),
		)

		It("is exact for a mixed adversarial set", func() {
			texts := []string{
				"plain", "a<b>c&d", `"q"`, "\u0000\u001f", "多字节", "\U0001F600", "\u2028",
			}
			gs := make([]protocol.Goroutine, 0, 512)
			for i := 1; i <= 512; i++ {
				g := packGoroutine(i, 0)
				g.CurrentLoc.File = strings.Repeat(texts[i%len(texts)], 3)
				g.StartLoc.Function = texts[(i+3)%len(texts)]
				gs = append(gs, g)
			}
			out, report := protocol.PackGoroutines(gs, false)
			expectReportedSize(protocol.EventGoroutines, out, report)
		})
	})

	Describe("adversarial element sizes", func() {
		It("packs maximum-length strings without mutating any Location", func() {
			// 4096 is the consumer's per-string limit; elements at that size must
			// be dropped whole, never silently rewritten to fit.
			long := strings.Repeat("s", 4096)
			gs := make([]protocol.Goroutine, 0, 8192)
			for i := 1; i <= 8192; i++ {
				g := packGoroutine(i, 0)
				g.CurrentLoc.File = long
				gs = append(gs, g)
			}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Threads: packThreads(16), Current: 1,
			}, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).NotTo(BeEmpty())
			for _, g := range out.Goroutines {
				Expect(g.CurrentLoc.File).To(Equal(long), "Location must survive untouched")
			}
			Expect(eventBytes(protocol.EventGoroutineSnapshot, out)).
				To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
		})

		It("skips an oversized element and keeps packing the rest", func() {
			// Lean elements so the whole set comfortably fits: the only reason
			// anything is dropped must be the two pathological entries.
			gs := make([]protocol.Goroutine, 0, 4000)
			for i := 1; i <= 4000; i++ {
				gs = append(gs, protocol.Goroutine{ID: i, Status: "runnable"})
			}
			huge := strings.Repeat("h", protocol.MaxGoroutineEventBytes)
			gs[1].CurrentLoc.File = huge
			gs[2].CurrentLoc.File = huge
			gs[0].Current = true

			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Current: 1,
			}, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).To(HaveLen(3998), "only the two huge elements are dropped")
			Expect(goroutineIDs(out.Goroutines)).NotTo(ContainElement(2))
			Expect(goroutineIDs(out.Goroutines)).NotTo(ContainElement(3))
			Expect(goroutineIDs(out.Goroutines)).To(ContainElement(4000))
		})

		It("degrades to empty collections when the anchor alone cannot fit", func() {
			gs := packGoroutines(4)
			gs[0].Current = true
			gs[0].CurrentLoc.File = strings.Repeat("h", 2*protocol.MaxGoroutineEventBytes)

			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Threads: packThreads(4), Current: 1, Created: []int{9},
			}, false, false)

			Expect(report.Degraded).To(BeTrue())
			Expect(out.Goroutines).To(BeEmpty())
			Expect(out.Threads).To(BeEmpty())
			Expect(out.Created).To(Equal([]int{9}), "lifecycle deltas are never trimmed")
			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Goroutines).To(Equal(4))
		})
	})

	Describe("exact budget boundaries", func() {
		// Every element string is capped, so a payload can only approach the
		// byte budget through MANY elements. padTo builds a snapshot whose
		// marshalled Event is EXACTLY size bytes out of legal elements: an
		// anchor, a run of fixed-size fillers, and one tail element tuned to
		// land on the byte. Only exact accounting can satisfy it.
		fillerText := strings.Repeat("w", 4000)
		build := func(fillers int, tail string) protocol.GoroutineSnapshotPayload {
			gs := []protocol.Goroutine{{ID: 1, Status: "running", Current: true}}
			for i := 2; i <= fillers+1; i++ {
				gs = append(gs, protocol.Goroutine{ID: i, Status: "waiting", WaitReason: fillerText})
			}
			if tail != "" {
				gs = append(gs, protocol.Goroutine{ID: 900000, Status: "waiting", WaitReason: tail})
			}
			return protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Threads: []protocol.Thread{}, Current: 1,
			}
		}
		padTo := func(size int) protocol.GoroutineSnapshotPayload {
			anchorOnly := eventBytes(protocol.EventGoroutineSnapshot, build(0, ""))
			perFiller := eventBytes(protocol.EventGoroutineSnapshot, build(1, "")) - anchorOnly
			for fillers := (size - anchorOnly) / perFiller; fillers >= 0; fillers-- {
				body := eventBytes(protocol.EventGoroutineSnapshot, build(fillers, ""))
				oneChar := eventBytes(protocol.EventGoroutineSnapshot, build(fillers, "x")) - body
				length := size - body - oneChar + 1
				if length < 1 || length > protocol.MaxGoroutineStringLength {
					continue
				}
				snap := build(fillers, strings.Repeat("t", length))
				Expect(eventBytes(protocol.EventGoroutineSnapshot, snap)).To(Equal(size))
				return snap
			}
			Fail("could not build a payload of exactly the requested size")
			return protocol.GoroutineSnapshotPayload{}
		}

		It("keeps everything one byte below the cap", func() {
			snap := padTo(protocol.MaxGoroutineEventBytes - 1)
			out, report := protocol.PackSnapshot(snap, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).To(HaveLen(len(snap.Goroutines)))
			Expect(out.Totals).To(BeNil(), "nothing was omitted")
			Expect(report.Bytes).To(Equal(protocol.MaxGoroutineEventBytes - 1))
		})

		It("keeps everything exactly at the cap", func() {
			snap := padTo(protocol.MaxGoroutineEventBytes)
			out, report := protocol.PackSnapshot(snap, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).To(HaveLen(len(snap.Goroutines)), "the cap is inclusive")
			Expect(out.Totals).To(BeNil())
			Expect(report.Bytes).To(Equal(protocol.MaxGoroutineEventBytes))
		})

		It("drops exactly one element one byte above the cap", func() {
			snap := padTo(protocol.MaxGoroutineEventBytes + 1)
			out, report := protocol.PackSnapshot(snap, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).To(HaveLen(len(snap.Goroutines) - 1))
			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Goroutines).To(Equal(len(snap.Goroutines)))
			Expect(report.Bytes).To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
		})

		It("drops only the over-budget non-anchor at the boundary", func() {
			anchor := protocol.Goroutine{ID: 1, Status: "running", Current: true}
			snap := protocol.GoroutineSnapshotPayload{
				Goroutines: []protocol.Goroutine{anchor, {ID: 2, Status: "waiting"}},
				Threads:    []protocol.Thread{},
				Current:    1,
			}
			base := eventBytes(protocol.EventGoroutineSnapshot, snap)
			// Widen the non-anchor by exactly one byte more than the headroom,
			// using deltas (not a string) since strings are separately capped.
			headroom := protocol.MaxGoroutineEventBytes - base
			created := make([]int, 0)
			for i := 0; len(created)*21 < headroom+64; i++ {
				created = append(created, math.MaxInt64-i)
			}
			snap.Created = created
			over, _ := protocol.PackSnapshot(snap, false, false)
			Expect(len(over.Goroutines)).To(BeNumerically("<=", 2))
			Expect(eventBytes(protocol.EventGoroutineSnapshot, over)).
				To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
		})
	})

	Describe("required anchors", func() {
		// chain builds current -> parent -> ... -> root of the given depth.
		chain := func(depth int, padding int) []protocol.Goroutine {
			gs := make([]protocol.Goroutine, 0, depth)
			for i := 1; i <= depth; i++ {
				g := protocol.Goroutine{
					ID: i, Status: "waiting",
					WaitReason: strings.Repeat("w", padding),
				}
				if i > 1 {
					g.ParentID = i - 1
				}
				gs = append(gs, g)
			}
			gs[depth-1].Current = true // deepest goroutine is the current one
			return gs
		}

		It("retains the entire ancestor chain, nearest-first", func() {
			gs := chain(40, 0)
			// Bury the chain among many unrelated goroutines competing for room.
			for i := 100; i < 4000; i++ {
				gs = append(gs, packGoroutine(i, 0))
			}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Current: 40,
			}, false, false)

			Expect(report.Degraded).To(BeFalse())
			ids := goroutineIDs(out.Goroutines)
			Expect(ids[0]).To(Equal(40), "current goroutine first")
			Expect(ids[1:40]).To(Equal([]int{
				39, 38, 37, 36, 35, 34, 33, 32, 31, 30, 29, 28, 27, 26, 25, 24, 23, 22, 21, 20,
				19, 18, 17, 16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1,
			}), "every ancestor, nearest-first")
		})

		It("degrades rather than dropping an ancestor that cannot fit", func() {
			// A chain whose own bytes exceed the budget: no conforming result
			// can keep every anchor, so nothing is emitted rather than a tree
			// with a hole in it.
			gs := chain(600, 4096)
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Current: 600,
			}, false, false)

			Expect(report.Degraded).To(BeTrue())
			Expect(out.Goroutines).To(BeEmpty())
			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Goroutines).To(Equal(600))
		})

		It("does not require ancestors in the flat list shape", func() {
			// EventGoroutines is rendered by DAP as a flat thread list — there
			// is no hierarchy to break. Degrading it to empty would make the DAP
			// translator fabricate a synthetic "main" thread, which is exactly
			// the lie issue #194 exists to stop.
			gs := chain(600, 4096)
			out, report := protocol.PackGoroutines(gs, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).NotTo(BeEmpty())
			Expect(out.Goroutines[0].ID).To(Equal(600), "the current goroutine is still required")
			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Goroutines).To(Equal(600))
		})

		It("keeps ancestor ordering in the flat list shape", func() {
			gs := chain(40, 0)
			for i := 100; i < 500; i++ {
				gs = append(gs, packGoroutine(i, 0))
			}
			out, _ := protocol.PackGoroutines(gs, false)

			ids := goroutineIDs(out.Goroutines)
			Expect(ids[0]).To(Equal(40))
			Expect(ids[1:5]).To(Equal([]int{39, 38, 37, 36}), "ancestors are still ordered first")
		})

		It("degrades when the ancestor chain exceeds the count cap", func() {
			gs := chain(protocol.MaxSnapshotGoroutines+1, 0)
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Current: protocol.MaxSnapshotGoroutines + 1,
			}, false, false)

			Expect(report.Degraded).To(BeTrue())
			Expect(out.Goroutines).To(BeEmpty())
		})

		DescribeTable("never exceeds the decoder's count cap on a deep chain",
			func(depth int) {
				// Anchors are placed before the per-element cap check, so the
				// cap must also be enforced on the anchor COUNT — otherwise a
				// long spawn chain yields a payload under 2 MiB but over the
				// element cap, which the consumer fatally rejects.
				gs := chain(depth, 0)

				snap, snapReport := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
					Goroutines: gs, Threads: []protocol.Thread{}, Current: depth,
				}, false, false)
				Expect(len(snap.Goroutines)).To(BeNumerically("<=", protocol.MaxSnapshotGoroutines))
				Expect(snapReport.Degraded).To(Equal(depth > protocol.MaxSnapshotGoroutines),
					"over the cap the snapshot must degrade explicitly")

				// The flat shape does not require ancestors, so it simply packs
				// up to the cap instead of degrading.
				list, listReport := protocol.PackGoroutines(gs, false)
				Expect(len(list.Goroutines)).To(BeNumerically("<=", protocol.MaxSnapshotGoroutines))
				Expect(listReport.Degraded).To(BeFalse())
				if depth > protocol.MaxSnapshotGoroutines {
					Expect(list.Goroutines).To(HaveLen(protocol.MaxSnapshotGoroutines))
					Expect(list.Totals).NotTo(BeNil())
					Expect(list.Totals.Goroutines).To(Equal(depth))
				}
			},
			Entry("exactly at the cap", protocol.MaxSnapshotGoroutines),
			Entry("one over the cap", protocol.MaxSnapshotGoroutines+1),
			Entry("at the scan ceiling", 8192),
		)

		It("drops the current goid when the result degrades", func() {
			// A degraded payload names no goroutine, so claiming one is current
			// is a dangling reference. Lifecycle deltas and totals survive.
			gs := chain(protocol.MaxSnapshotGoroutines+1, 0)
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs,
				Current:    protocol.MaxSnapshotGoroutines + 1,
				Created:    []int{7, 8},
				Exited:     []int{3},
			}, false, false)

			Expect(report.Degraded).To(BeTrue())
			Expect(out.Goroutines).To(BeEmpty())
			Expect(out.Current).To(BeZero(), "no goroutine was delivered to be current")
			Expect(out.Created).To(Equal([]int{7, 8}))
			Expect(out.Exited).To(Equal([]int{3}))
			Expect(out.Totals).NotTo(BeNil())

			raw, err := json.Marshal(out)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw)).NotTo(ContainSubstring(`"current"`), "omitempty drops it entirely")
		})

		It("drops a current goid that names no goroutine in the input", func() {
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: []protocol.Goroutine{{ID: 1, Status: "waiting"}},
				Threads:    []protocol.Thread{},
				Current:    999,
			}, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Current).To(BeZero())
			Expect(goroutineIDs(out.Goroutines)).To(Equal([]int{1}))
		})

		It("keeps the current goid in every non-degraded result", func() {
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192), Threads: packThreads(8), Current: 4096,
			}, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Current).To(Equal(4096))
			Expect(goroutineIDs(out.Goroutines)).To(ContainElement(4096))
		})

		It("keeps an ancestor chain that exactly fills the count cap", func() {
			gs := chain(protocol.MaxSnapshotGoroutines, 0)
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Current: protocol.MaxSnapshotGoroutines,
			}, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).To(HaveLen(protocol.MaxSnapshotGoroutines))
		})

		It("stops the anchor chain at an unknown parent", func() {
			gs := []protocol.Goroutine{
				{ID: 5, ParentID: 999, Status: "running", Current: true},
				{ID: 6, Status: "waiting"},
			}
			out, report := protocol.PackGoroutines(gs, false)
			Expect(report.Degraded).To(BeFalse())
			Expect(goroutineIDs(out.Goroutines)).To(Equal([]int{5, 6}))
		})
	})

	Describe("per-element string limit", func() {
		// The consumer rejects any string longer than MaxGoroutineStringLength
		// in UTF-16 code units. One such string is nowhere near the byte budget,
		// so budgeting alone would emit an element the consumer MUST reject —
		// killing that connection deterministically on every retry. The producer
		// therefore drops the element whole; it never truncates.
		ascii := func(n int) string { return strings.Repeat("a", n) }
		// U+1F600 is astral: one rune, TWO UTF-16 code units.
		astral := func(units int) string { return strings.Repeat("\U0001F600", units/2) }

		nonAnchor := func(mutate func(*protocol.Goroutine)) (protocol.GoroutineSnapshotPayload, protocol.GoroutinePackReport) {
			victim := protocol.Goroutine{ID: 2, Status: "waiting"}
			mutate(&victim)
			return protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: []protocol.Goroutine{
					{ID: 1, Status: "running", Current: true},
					victim,
				},
				Threads: []protocol.Thread{},
				Current: 1,
			}, false, false)
		}

		DescribeTable("accepts a string exactly at the limit",
			func(mutate func(*protocol.Goroutine)) {
				out, report := nonAnchor(mutate)
				Expect(report.Degraded).To(BeFalse())
				Expect(goroutineIDs(out.Goroutines)).To(Equal([]int{1, 2}))
				Expect(out.Totals).To(BeNil(), "nothing was dropped")
			},
			Entry("status", func(g *protocol.Goroutine) { g.Status = ascii(4096) }),
			Entry("waitReason", func(g *protocol.Goroutine) { g.WaitReason = ascii(4096) }),
			Entry("currentLoc.file", func(g *protocol.Goroutine) { g.CurrentLoc.File = ascii(4096) }),
			Entry("currentLoc.function", func(g *protocol.Goroutine) { g.CurrentLoc.Function = ascii(4096) }),
			Entry("startLoc.file", func(g *protocol.Goroutine) { g.StartLoc.File = ascii(4096) }),
			Entry("startLoc.function", func(g *protocol.Goroutine) { g.StartLoc.Function = ascii(4096) }),
			Entry("createdLoc.file", func(g *protocol.Goroutine) { g.CreatedLoc.File = ascii(4096) }),
			Entry("createdLoc.function", func(g *protocol.Goroutine) { g.CreatedLoc.Function = ascii(4096) }),
			Entry("astral at the limit", func(g *protocol.Goroutine) { g.CurrentLoc.File = astral(4096) }),
		)

		DescribeTable("skips a non-anchor one unit over the limit",
			func(mutate func(*protocol.Goroutine)) {
				out, report := nonAnchor(mutate)
				Expect(report.Degraded).To(BeFalse())
				Expect(goroutineIDs(out.Goroutines)).To(Equal([]int{1}), "skip and continue")
				Expect(out.Totals).NotTo(BeNil())
				Expect(out.Totals.Goroutines).To(Equal(2))
			},
			Entry("status", func(g *protocol.Goroutine) { g.Status = ascii(4097) }),
			Entry("waitReason", func(g *protocol.Goroutine) { g.WaitReason = ascii(4097) }),
			Entry("currentLoc.file", func(g *protocol.Goroutine) { g.CurrentLoc.File = ascii(4097) }),
			Entry("currentLoc.function", func(g *protocol.Goroutine) { g.CurrentLoc.Function = ascii(4097) }),
			Entry("startLoc.file", func(g *protocol.Goroutine) { g.StartLoc.File = ascii(4097) }),
			Entry("startLoc.function", func(g *protocol.Goroutine) { g.StartLoc.Function = ascii(4097) }),
			Entry("createdLoc.file", func(g *protocol.Goroutine) { g.CreatedLoc.File = ascii(4097) }),
			Entry("createdLoc.function", func(g *protocol.Goroutine) { g.CreatedLoc.Function = ascii(4097) }),
			Entry("astral one unit over", func(g *protocol.Goroutine) {
				g.CurrentLoc.File = astral(4096) + "a" // 4096 units + 1
			}),
		)

		It("counts astral characters as two units, not one", func() {
			// 2049 astral runes = 4098 UTF-16 units. Rune-counting would call
			// this legal and hand the consumer something it must reject.
			over := strings.Repeat("\U0001F600", 2049)
			Expect(len([]rune(over))).To(BeNumerically("<", protocol.MaxGoroutineStringLength))
			out, _ := nonAnchor(func(g *protocol.Goroutine) { g.CurrentLoc.File = over })
			Expect(goroutineIDs(out.Goroutines)).To(Equal([]int{1}))

			// 2048 astral runes = exactly 4096 units, so it is legal.
			atLimit := strings.Repeat("\U0001F600", 2048)
			kept, _ := nonAnchor(func(g *protocol.Goroutine) { g.CurrentLoc.File = atLimit })
			Expect(goroutineIDs(kept.Goroutines)).To(Equal([]int{1, 2}))
		})

		It("rejects a string of MaxGoroutineStringLength astral code points", func() {
			// The case a rune-based limit gets exactly backwards: 4096 astral
			// code points look like "exactly at the limit" but are 8192 UTF-16
			// units on the wire, which the consumer must refuse.
			full := strings.Repeat("\U0001F600", protocol.MaxGoroutineStringLength)
			Expect(len([]rune(full))).To(Equal(protocol.MaxGoroutineStringLength))
			Expect(utf16Units(full)).To(Equal(2 * protocol.MaxGoroutineStringLength))

			out, report := nonAnchor(func(g *protocol.Goroutine) { g.CurrentLoc.File = full })
			Expect(report.Degraded).To(BeFalse())
			Expect(goroutineIDs(out.Goroutines)).To(Equal([]int{1}), "dropped, not emitted")
			for _, g := range out.Goroutines {
				Expect(g.CurrentLoc.File).NotTo(ContainSubstring("\U0001F600"))
			}
		})

		DescribeTable("counts invalid UTF-8 the way the wire renders it",
			func(build func(n int) string, unitsPerRepeat int) {
				// encoding/json substitutes one U+FFFD per invalid byte, and a
				// range loop yields one RuneError per invalid byte, so the two
				// agree. A limit that counted BYTES would disagree here.
				fits := protocol.MaxGoroutineStringLength / unitsPerRepeat
				atLimit := build(fits)
				Expect(utf16Units(atLimit)).To(BeNumerically("<=", protocol.MaxGoroutineStringLength))
				Expect(utf16Units(atLimit)).To(BeNumerically(">", protocol.MaxGoroutineStringLength-unitsPerRepeat))
				kept, _ := nonAnchor(func(g *protocol.Goroutine) { g.CurrentLoc.File = atLimit })
				Expect(goroutineIDs(kept.Goroutines)).To(Equal([]int{1, 2}), "the largest fitting string is kept")

				over := build(fits + 1)
				Expect(utf16Units(over)).To(BeNumerically(">", protocol.MaxGoroutineStringLength))
				dropped, _ := nonAnchor(func(g *protocol.Goroutine) { g.CurrentLoc.File = over })
				Expect(goroutineIDs(dropped.Goroutines)).To(Equal([]int{1}), "one repeat more is dropped")
			},
			Entry("bare invalid bytes", func(n int) string { return strings.Repeat("\xff", n) }, 1),
			Entry("truncated multi-byte sequences", func(n int) string {
				return strings.Repeat("\xe2\x80", n)
			}, 2),
			Entry("encoded surrogate halves", func(n int) string {
				return strings.Repeat("\xed\xa0\x80", n)
			}, 3),
		)

		It("agrees with the post-wire length for every escape class", func() {
			// The property the whole limit rests on: what Go counts is what the
			// consumer will measure after the value has been through JSON.
			for _, s := range []string{
				strings.Repeat("a", 4096),
				strings.Repeat("\U0001F600", 2048),
				strings.Repeat("\xff", 100),
				strings.Repeat("\xe2\x80", 50),
				"a<b>c&d\"e\\f",
				"\u2028\u2029\u0000\u001f",
				"服务 café \U0001F680",
			} {
				raw, err := json.Marshal(s)
				Expect(err).NotTo(HaveOccurred())
				var wire string
				Expect(json.Unmarshal(raw, &wire)).To(Succeed())
				Expect(utf16Units(s)).To(Equal(utf16Units(wire)),
					"the producer's count must survive the round trip: %q", s)
			}
		})

		It("degrades when the current goroutine violates the limit", func() {
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: []protocol.Goroutine{
					{ID: 1, Status: "running", Current: true,
						CurrentLoc: protocol.Location{File: ascii(4097)}},
					{ID: 2, Status: "waiting"},
				},
				Threads: []protocol.Thread{},
				Current: 1,
			}, false, false)

			Expect(report.Degraded).To(BeTrue(), "an anchor cannot be skipped")
			Expect(out.Goroutines).To(BeEmpty())
		})

		It("degrades when an ancestor anchor violates the limit", func() {
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: []protocol.Goroutine{
					{ID: 1, Status: "waiting", CurrentLoc: protocol.Location{File: ascii(4097)}},
					{ID: 2, ParentID: 1, Status: "running", Current: true},
				},
				Threads: []protocol.Thread{},
				Current: 2,
			}, false, false)

			Expect(report.Degraded).To(BeTrue())
			Expect(out.Goroutines).To(BeEmpty())
		})

		It("skips an over-limit ancestor in the flat list shape", func() {
			out, report := protocol.PackGoroutines([]protocol.Goroutine{
				{ID: 1, Status: "waiting", CurrentLoc: protocol.Location{File: ascii(4097)}},
				{ID: 2, ParentID: 1, Status: "running", Current: true},
			}, false)

			Expect(report.Degraded).To(BeFalse(), "ancestors are not required here")
			Expect(goroutineIDs(out.Goroutines)).To(Equal([]int{2}))
		})

		It("skips an over-limit thread and degrades on the current one", func() {
			long := protocol.Location{File: ascii(4097)}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: []protocol.Goroutine{{ID: 1, Status: "running", Current: true}},
				Threads: []protocol.Thread{
					{ID: 10, MID: 0, Current: true},
					{ID: 11, MID: 1, CurrentLoc: long},
					{ID: 12, MID: 2},
				},
				Current: 1,
			}, false, false)
			Expect(report.Degraded).To(BeFalse())
			Expect(threadMIDs(out.Threads)).To(Equal([]int{0, 2}))

			_, currentBad := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: []protocol.Goroutine{{ID: 1, Status: "running", Current: true}},
				Threads:    []protocol.Thread{{ID: 10, MID: 0, Current: true, CurrentLoc: long}},
				Current:    1,
			}, false, false)
			Expect(currentBad.Degraded).To(BeTrue())
		})

		It("never emits a string over the limit, whatever the input", func() {
			// The property that actually closes the loop with the consumer.
			gs := make([]protocol.Goroutine, 0, 400)
			for i := 1; i <= 400; i++ {
				g := packGoroutine(i, 0)
				switch i % 5 {
				case 0:
					g.CurrentLoc.File = ascii(4097 + i)
				case 1:
					g.StartLoc.Function = astral(8192)
				case 2:
					g.WaitReason = ascii(100000)
				case 3:
					g.Status = ascii(4096) // legal, must survive
				}
				gs = append(gs, g)
			}
			// The anchor must itself be legal, or the pack degrades before any
			// of the hostile non-anchors are even considered.
			gs[0] = protocol.Goroutine{ID: 1, Status: "running", Current: true}
			ts := []protocol.Thread{{ID: 1, Current: true}}
			for i := 2; i <= 40; i++ {
				ts = append(ts, protocol.Thread{ID: i, MID: i,
					CurrentLoc: protocol.Location{File: ascii(4097)}})
			}

			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Threads: ts, Current: 1,
			}, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).NotTo(BeEmpty())
			for _, g := range out.Goroutines {
				for _, s := range []string{g.Status, g.WaitReason,
					g.CurrentLoc.File, g.CurrentLoc.Function,
					g.StartLoc.File, g.StartLoc.Function,
					g.CreatedLoc.File, g.CreatedLoc.Function} {
					Expect(utf16Units(s)).To(BeNumerically("<=", protocol.MaxGoroutineStringLength))
				}
			}
			for _, t := range out.Threads {
				Expect(utf16Units(t.CurrentLoc.File)).To(BeNumerically("<=", protocol.MaxGoroutineStringLength))
				Expect(utf16Units(t.CurrentLoc.Function)).To(BeNumerically("<=", protocol.MaxGoroutineStringLength))
			}
		})
	})

	Describe("deterministic selection", func() {
		It("keeps the current goroutine, its ancestors nearest-first, then ascending goid", func() {
			gs := packGoroutines(8192)
			// 8000 -> 4000 -> 2000 -> 1000 -> ... -> 1 by the i/2 parent rule.
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Current: 8000,
			}, false, false)

			ids := goroutineIDs(out.Goroutines)
			Expect(ids[0]).To(Equal(8000))
			Expect(ids[1:13]).To(Equal([]int{4000, 2000, 1000, 500, 250, 125, 62, 31, 15, 7, 3, 1}))
			rest := ids[13:]
			for i := 1; i < len(rest); i++ {
				Expect(rest[i]).To(BeNumerically(">", rest[i-1]), "the remainder is ascending goid")
			}
		})

		It("retains the current goroutine even when it sorts last", func() {
			gs := packGoroutines(8192)
			out, _ := protocol.PackGoroutines(withCurrent(gs, 8192), false)

			Expect(out.Goroutines[0].ID).To(Equal(8192))
			Expect(len(out.Goroutines)).To(BeNumerically("<", 8192))
		})

		It("retains the current thread even when it sorts last", func() {
			ts := packThreads(64)
			ts[63].Current = true
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192), Threads: ts, Current: 1,
			}, false, false)

			Expect(out.Threads[0].MID).To(Equal(63))
			Expect(out.Threads[0].Current).To(BeTrue())
		})

		It("produces byte-identical output for the same input", func() {
			snap := protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192), Threads: packThreads(64), Current: 4096,
			}
			first, firstReport := protocol.PackSnapshot(snap, false, false)
			second, secondReport := protocol.PackSnapshot(snap, false, false)

			firstRaw, err := json.Marshal(first)
			Expect(err).NotTo(HaveOccurred())
			secondRaw, err := json.Marshal(second)
			Expect(err).NotTo(HaveOccurred())
			Expect(firstRaw).To(Equal(secondRaw))
			Expect(firstReport).To(Equal(secondReport))
		})

		It("does not loop on a parent cycle", func() {
			gs := []protocol.Goroutine{
				{ID: 1, ParentID: 2, Status: "running", Current: true},
				{ID: 2, ParentID: 1, Status: "waiting"},
			}
			out, report := protocol.PackGoroutines(gs, false)
			Expect(report.Degraded).To(BeFalse())
			Expect(goroutineIDs(out.Goroutines)).To(Equal([]int{1, 2}))
		})
	})

	Describe("count caps", func() {
		It("caps goroutines even when the bytes would allow more", func() {
			lean := make([]protocol.Goroutine, 0, 8192)
			for i := 1; i <= 8192; i++ {
				lean = append(lean, protocol.Goroutine{ID: i, Status: "runnable"})
			}
			out, report := protocol.PackGoroutines(lean, false)

			Expect(out.Goroutines).To(HaveLen(protocol.MaxSnapshotGoroutines))
			Expect(report.Bytes).To(BeNumerically("<", protocol.MaxGoroutineEventBytes),
				"the cap must bind before the budget does")
			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Goroutines).To(Equal(8192))
		})

		It("caps threads even when the bytes would allow more", func() {
			lean := make([]protocol.Thread, 0, 4096)
			for i := 0; i < 4096; i++ {
				lean = append(lean, protocol.Thread{ID: i, MID: i})
			}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: []protocol.Goroutine{{ID: 1, Status: "running", Current: true}},
				Threads:    lean,
				Current:    1,
			}, false, false)

			Expect(out.Threads).To(HaveLen(protocol.MaxSnapshotThreads))
			Expect(report.Bytes).To(BeNumerically("<", protocol.MaxGoroutineEventBytes))
		})
	})

	Describe("thread floor and reclaim", func() {
		DescribeTable("keeps a realistic thread set whole",
			func(threads int) {
				out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
					Goroutines: packGoroutines(1500),
					Threads:    packThreads(threads),
					Current:    1,
				}, false, false)

				Expect(out.Threads).To(HaveLen(threads))
				Expect(out.Goroutines).To(HaveLen(1500))
				Expect(report.Omitted()).To(BeFalse())
				Expect(out.Totals).To(BeNil())
			},
			Entry("8 threads", 8),
			Entry("16 threads", 16),
			Entry("32 threads", 32),
			Entry("64 threads", 64),
		)

		It("guarantees the floor even when goroutines saturate the budget", func() {
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192),
				Threads:    packThreads(64),
				Current:    1,
			}, false, false)

			Expect(len(out.Threads)).To(BeNumerically(">=", protocol.MinThreadsRetained))
			Expect(threadMIDs(out.Threads)[:protocol.MinThreadsRetained]).
				To(Equal(threadMIDs(packThreads(protocol.MinThreadsRetained))))
		})

		It("reclaims leftover budget for threads beyond the floor", func() {
			// The goroutine set fits with room to spare, so every thread above
			// the floor is reclaimed rather than dropped.
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(100),
				Threads:    packThreads(300),
				Current:    1,
			}, false, false)

			Expect(out.Threads).To(HaveLen(300))
		})
	})

	Describe("totals", func() {
		It("is absent on a complete unclipped snapshot", func() {
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(10), Threads: packThreads(4), Current: 1,
			}, false, false)

			Expect(out.Totals).To(BeNil())
			Expect(report.Omitted()).To(BeFalse())
			Expect(report.Totals.Goroutines).To(Equal(10), "the report still knows the truth")
		})

		It("is absent on a complete unclipped goroutine list", func() {
			out, _ := protocol.PackGoroutines(packGoroutines(10), false)
			Expect(out.Totals).To(BeNil())
		})

		It("is present with original counts when the snapshot omits elements", func() {
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192), Threads: packThreads(64), Current: 1,
			}, false, false)

			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Goroutines).To(Equal(8192))
			Expect(out.Totals.Threads).To(Equal(64))
			Expect(out.Totals.GoroutinesClipped).To(BeFalse())
			Expect(out.Totals.ThreadsClipped).To(BeFalse())
			Expect(report.Omitted()).To(BeTrue())
		})

		It("is present with original counts when the goroutine list omits elements", func() {
			out, _ := protocol.PackGoroutines(packGoroutines(8192), false)

			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Goroutines).To(Equal(8192))
			Expect(out.Totals.Threads).To(BeZero(), "this shape carries no threads")
		})

		It("is present on a complete snapshot when a scan was clipped", func() {
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(10), Threads: packThreads(4), Current: 1,
			}, true, true)

			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.GoroutinesClipped).To(BeTrue())
			Expect(out.Totals.Goroutines).To(Equal(10), "a lower bound, flagged as such")
		})

		DescribeTable("reports each scan's clipping independently",
			func(goroutinesClipped, threadsClipped bool) {
				out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
					Goroutines: packGoroutines(10), Threads: packThreads(4), Current: 1,
				}, goroutinesClipped, threadsClipped)

				if !goroutinesClipped && !threadsClipped {
					Expect(out.Totals).To(BeNil(), "a complete unclipped result sends nothing")
					Expect(report.Totals.AnyClipped()).To(BeFalse())
					return
				}
				Expect(out.Totals).NotTo(BeNil())
				Expect(out.Totals.GoroutinesClipped).To(Equal(goroutinesClipped))
				Expect(out.Totals.ThreadsClipped).To(Equal(threadsClipped))
				// The counts stay exact scanned totals either way; only the
				// flags say which of them is a floor.
				Expect(out.Totals.Goroutines).To(Equal(10))
				Expect(out.Totals.Threads).To(Equal(4))
				Expect(report.Totals.AnyClipped()).To(BeTrue())
			},
			Entry("neither", false, false),
			Entry("goroutines only", true, false),
			Entry("threads only", false, true),
			Entry("both", true, true),
		)

		It("carries only the goroutine flag in the flat list shape", func() {
			clipped, _ := protocol.PackGoroutines(packGoroutines(10), true)
			Expect(clipped.Totals).NotTo(BeNil())
			Expect(clipped.Totals.GoroutinesClipped).To(BeTrue())
			Expect(clipped.Totals.ThreadsClipped).To(BeFalse(), "this shape has no threads")
			Expect(clipped.Totals.Threads).To(BeZero())

			plain, _ := protocol.PackGoroutines(packGoroutines(10), false)
			Expect(plain.Totals).To(BeNil())
		})

		It("sends totals when only the thread scan clipped", func() {
			// The regression a single shared flag hides: nothing was omitted and
			// the goroutine count is exact, but the thread count is not.
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(10), Threads: packThreads(4), Current: 1,
			}, false, true)

			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.GoroutinesClipped).To(BeFalse())
			Expect(out.Totals.ThreadsClipped).To(BeTrue())
		})

		It("is present on a complete goroutine list when the scan was clipped", func() {
			out, _ := protocol.PackGoroutines(packGoroutines(10), true)

			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.GoroutinesClipped).To(BeTrue())
		})
	})

	Describe("lifecycle deltas", func() {
		It("never trims created or exited, even at max int64", func() {
			created := []int{math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64 - 2}
			exited := []int{math.MaxInt64 - 3, math.MaxInt64 - 4}
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192),
				Threads:    packThreads(64),
				Current:    1,
				Created:    created,
				Exited:     exited,
			}, false, false)

			Expect(out.Created).To(Equal(created))
			Expect(out.Exited).To(Equal(exited))
			Expect(eventBytes(protocol.EventGoroutineSnapshot, out)).
				To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
		})

		It("keeps deltas intact through a degraded result", func() {
			gs := []protocol.Goroutine{{
				ID: 1, Status: "running", Current: true,
				CurrentLoc: protocol.Location{File: strings.Repeat("x", 4*protocol.MaxGoroutineEventBytes)},
			}}
			created := make([]int, 2000)
			for i := range created {
				created[i] = math.MaxInt64 - i
			}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Current: 1, Created: created,
			}, false, false)

			Expect(report.Degraded).To(BeTrue())
			Expect(report.Oversized).To(BeFalse())
			Expect(out.Created).To(HaveLen(2000))
		})

		It("packs the largest delta the runtime scan can produce", func() {
			// created/exited are bounded by the debugger's goroutine scan
			// ceiling, so the packer must still conform at exactly that size.
			created := make([]int, protocol.MaxLifecycleDeltaIDs)
			exited := make([]int, protocol.MaxLifecycleDeltaIDs)
			for i := range created {
				// Stay inside JavaScript's safe-integer range: the consumer
				// rejects anything above it, so a "worst case" built from
				// unrepresentable ids would not be a realistic worst case.
				created[i] = maxSafeGoid - i
				exited[i] = maxSafeGoid - protocol.MaxLifecycleDeltaIDs - i
			}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192),
				Threads:    packThreads(64),
				Current:    1,
				Created:    created,
				Exited:     exited,
			}, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(report.Oversized).To(BeFalse())
			Expect(out.Created).To(HaveLen(protocol.MaxLifecycleDeltaIDs))
			Expect(out.Exited).To(HaveLen(protocol.MaxLifecycleDeltaIDs))
			Expect(out.Goroutines).NotTo(BeEmpty(), "deltas must not starve the elements")
			Expect(eventBytes(protocol.EventGoroutineSnapshot, out)).
				To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
		})

		It("leaves the worst-case deltas well inside the byte budget", func() {
			// Both deltas at the ceiling with maximum-width ids, and no elements
			// at all: the floor the packer can never go below.
			created := make([]int, protocol.MaxLifecycleDeltaIDs)
			exited := make([]int, protocol.MaxLifecycleDeltaIDs)
			for i := range created {
				created[i] = maxSafeGoid - i
				exited[i] = maxSafeGoid - protocol.MaxLifecycleDeltaIDs - i
			}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: []protocol.Goroutine{{ID: 1, Status: "running", Current: true}},
				Threads:    []protocol.Thread{},
				Current:    1,
				Created:    created,
				Exited:     exited,
			}, false, false)

			Expect(report.Oversized).To(BeFalse())
			Expect(report.Degraded).To(BeFalse())
			size := eventBytes(protocol.EventGoroutineSnapshot, out)
			Expect(size).To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
			Expect(size).To(BeNumerically("<", protocol.MaxGoroutineEventBytes/2),
				"the worst case must leave real room for elements, not just fit")
		})

		It("reports rather than trims a delta beyond the scan ceiling", func() {
			// Not reachable from the real producer, but the packer must never
			// silently drop lifecycle events to make a payload conform.
			created := make([]int, protocol.MaxLifecycleDeltaIDs+1)
			for i := range created {
				created[i] = i + 1
			}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: []protocol.Goroutine{{ID: 1, Status: "running", Current: true}},
				Current:    1,
				Created:    created,
			}, false, false)

			Expect(report.Oversized).To(BeTrue())
			Expect(out.Created).To(HaveLen(protocol.MaxLifecycleDeltaIDs+1), "never trimmed")
		})

		It("matches the debugger's goroutine scan ceiling", func() {
			// The delta bound is only correct while it equals the scan that
			// produces the deltas. If layer B retunes the scan, this fails and
			// the wire contract (and the consumer's mirror of it) must follow.
			source, err := os.ReadFile(filepath.Join("..", "..", "internal", "debugger", "goroutines.go"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(source)).To(ContainSubstring(
				"maxGoroutineScan = " + strconv.Itoa(protocol.MaxLifecycleDeltaIDs)))
		})

		It("reports oversized when the deltas alone cannot fit", func() {
			// Not reachable from the real producer (its scan bounds the deltas),
			// but the packer must not silently claim a conforming result.
			created := make([]int, 200_000)
			for i := range created {
				created[i] = math.MaxInt64 - i
			}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(4), Current: 1, Created: created,
			}, false, false)

			Expect(report.Degraded).To(BeTrue())
			Expect(report.Oversized).To(BeTrue())
			Expect(report.Bytes).To(BeNumerically(">", protocol.MaxGoroutineEventBytes))
			Expect(out.Created).To(HaveLen(200_000), "deltas are never trimmed")
		})
	})

	Describe("cost model", func() {
		It("marshals each candidate at most twice and the payload a constant number of times", func() {
			gs := packGoroutines(8192)
			ts := packThreads(64)
			protocol.ResetPackMarshalCounts()
			protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Threads: ts, Current: 1,
			}, false, false)
			elements, envelopes := protocol.PackMarshalCounts()

			Expect(elements).To(BeNumerically("<=", 2*(len(gs)+len(ts))))
			Expect(envelopes).To(BeNumerically("<=", 4),
				"a per-candidate whole-payload marshal would be O(n^2)")
		})

		It("marshals no elements at all when the payload already fits", func() {
			// A snapshot is packed on EVERY breakpoint stop, so the common case
			// must not pay per element. Paying it there once pushed the churn
			// e2e past its target's watchdog.
			lean := func(n int) []protocol.Goroutine {
				out := make([]protocol.Goroutine, 0, n)
				for i := 1; i <= n; i++ {
					out = append(out, protocol.Goroutine{ID: i, Status: "runnable"})
				}
				return out
			}

			for _, n := range []int{10, 1000, 4000} {
				protocol.ResetPackMarshalCounts()
				out, report := protocol.PackGoroutines(lean(n), false)
				elements, envelopes := protocol.PackMarshalCounts()

				Expect(out.Goroutines).To(HaveLen(n), "nothing is dropped")
				Expect(report.Omitted()).To(BeFalse())
				Expect(elements).To(BeZero(), "a payload that fits needs no per-element work")
				Expect(envelopes).To(BeZero(),
					"and no marshalling at all — the cheap bound already proved it fits")
				Expect(report.Bytes).To(BeZero(), "nothing was measured, so nothing is reported")
			}
		})

		It("does not pay for packing just because a scan clipped", func() {
			// Clipping means "attach the totals", not "something must be
			// trimmed". Treating it as the latter made a thread-churning target
			// — whose runtime.allm passes the scan ceiling — do full
			// per-element packing on every single stop, which cost enough to
			// push the churn e2e past its target's watchdog.
			snap := protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(20),
				Threads:    packThreads(2048),
				Current:    1,
			}
			protocol.ResetPackMarshalCounts()
			out, report := protocol.PackSnapshot(snap, false, true)
			elements, envelopes := protocol.PackMarshalCounts()

			Expect(report.Omitted()).To(BeFalse(), "nothing needed trimming")
			Expect(out.Threads).To(HaveLen(2048))
			Expect(elements).To(BeZero(), "a clipped scan must not force per-element work")
			Expect(envelopes).To(BeZero())
			Expect(out.Totals).NotTo(BeNil(), "but the totals must still be attached")
			Expect(out.Totals.ThreadsClipped).To(BeTrue())
			Expect(out.Totals.GoroutinesClipped).To(BeFalse())
			Expect(eventBytes(protocol.EventGoroutineSnapshot, out)).
				To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
		})

		It("falls back to per-element packing only when trimming is needed", func() {
			gs := packGoroutines(8192)
			protocol.ResetPackMarshalCounts()
			_, report := protocol.PackGoroutines(gs, false)
			elements, _ := protocol.PackMarshalCounts()

			Expect(report.Omitted()).To(BeTrue())
			Expect(elements).To(BeNumerically(">", 0))
			Expect(elements).To(BeNumerically("<=", 2*len(gs)))
		})

		It("costs at most one extra pass when the exact reserve must be retried", func() {
			gs := packGoroutines(8192)
			protocol.ResetPackMarshalCounts()
			_, report := protocol.PackGoroutines(gs, false)
			elements, envelopes := protocol.PackMarshalCounts()

			Expect(report.Omitted()).To(BeTrue(), "this input must trigger the retry")
			Expect(elements).To(BeNumerically("<=", 2*len(gs)))
			Expect(envelopes).To(BeNumerically("<=", 4))
		})
	})

	Describe("empty and degenerate inputs", func() {
		It("packs an empty snapshot without degrading", func() {
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{}, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).To(BeEmpty())
			Expect(out.Goroutines).NotTo(BeNil(), "empty collections marshal as [] not null")
			Expect(out.Threads).NotTo(BeNil())
			Expect(out.Totals).To(BeNil())
		})

		It("packs the degraded single synthetic goroutine untouched", func() {
			synthetic := []protocol.Goroutine{{ID: 1, Status: "waiting", Current: true}}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: synthetic,
			}, false, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).To(Equal(synthetic))
			Expect(out.Totals).To(BeNil())
		})
	})

	Describe("round trip", func() {
		It("survives the wire with totals", func() {
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192), Threads: packThreads(64), Current: 1,
			}, true, true)

			evt := protocol.MustEvent(protocol.EventGoroutineSnapshot, 7, out)
			raw, err := protocol.MarshalEvent(evt)
			Expect(err).NotTo(HaveOccurred())
			decodedEvent, err := protocol.UnmarshalEvent(raw)
			Expect(err).NotTo(HaveOccurred())

			var decoded protocol.GoroutineSnapshotPayload
			Expect(protocol.DecodeEventPayload(decodedEvent, &decoded)).To(Succeed())
			Expect(decoded.Totals).NotTo(BeNil())
			Expect(decoded.Totals.Goroutines).To(Equal(8192))
			Expect(decoded.Totals.GoroutinesClipped).To(BeTrue())
			Expect(goroutineIDs(decoded.Goroutines)).To(Equal(goroutineIDs(out.Goroutines)))
		})

		It("omits the totals key entirely when complete", func() {
			out, _ := protocol.PackGoroutines(packGoroutines(4), false)
			raw, err := json.Marshal(out)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw)).NotTo(ContainSubstring("totals"))
		})
	})
})

// utf16Units counts UTF-16 code units independently of the implementation under
// test, so the "never emits an over-limit string" property is not self-verified.
func utf16Units(s string) int {
	return len(utf16.Encode([]rune(s)))
}

func withCurrent(gs []protocol.Goroutine, id int) []protocol.Goroutine {
	out := make([]protocol.Goroutine, len(gs))
	copy(out, gs)
	for i := range out {
		out[i].Current = out[i].ID == id
	}
	return out
}

// The contract is now IN FORCE: both producers pack, the version advertises it,
// and consumers enforce it. These specs pin the activation as a whole, because
// the halves are only safe together — a version that promises boundedness while
// a producer still emits unbounded events lets a conforming client reject valid
// output, and enforcement without the bump rejects peers that never agreed.
var _ = Describe("the contract is active", func() {
	It("advertises the version that carries it", func() {
		Expect(protocol.Version).To(Equal("1.4"))
	})

	It("still omits totals from a complete result", func() {
		out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
			Goroutines: packGoroutines(4),
			Threads:    packThreads(2),
			Current:    1,
		}, false, false)
		Expect(report.Omitted()).To(BeFalse())

		raw, err := json.Marshal(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(ContainSubstring("totals"),
			"a complete result must stay byte-identical to the pre-1.4 shape")
	})

	It("still decodes a payload written without the new field", func() {
		// A peer that omits the optional field must keep decoding cleanly.
		var snap protocol.GoroutineSnapshotPayload
		Expect(json.Unmarshal([]byte(
			`{"goroutines":[],"threads":[],"current":1,"created":[7],"exited":[3]}`,
		), &snap)).To(Succeed())
		Expect(snap.Totals).To(BeNil())
		Expect(snap.Current).To(Equal(1))

		var list protocol.GoroutinesPayload
		Expect(json.Unmarshal([]byte(`{"goroutines":[]}`), &list)).To(Succeed())
		Expect(list.Totals).To(BeNil())
	})
})
