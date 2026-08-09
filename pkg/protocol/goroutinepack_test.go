package protocol_test

import (
	"encoding/json"
	"math"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/pkg/protocol"
)

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
			out, report := protocol.PackSnapshot(snap, false)

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
			}, false)
			loaded, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192), Current: 1, Created: created,
			}, false)

			Expect(len(loaded.Goroutines)).To(BeNumerically("<", len(bare.Goroutines)))
			Expect(eventBytes(protocol.EventGoroutineSnapshot, loaded)).
				To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
		})

		It("uses the same accounting for both payload shapes", func() {
			gs := packGoroutines(8192)
			snap, snapReport := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Current: 1,
			}, false)
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
			}, false)
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
				Expect(report.Bytes).To(Equal(eventBytes(protocol.EventGoroutines, out)))
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
			Expect(report.Bytes).To(Equal(eventBytes(protocol.EventGoroutines, out)))
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
			}, false)

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
			}, false)

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
			}, false)

			Expect(report.Degraded).To(BeTrue())
			Expect(out.Goroutines).To(BeEmpty())
			Expect(out.Threads).To(BeEmpty())
			Expect(out.Created).To(Equal([]int{9}), "lifecycle deltas are never trimmed")
			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Goroutines).To(Equal(4))
		})
	})

	Describe("deterministic selection", func() {
		It("keeps the current goroutine, its ancestors nearest-first, then ascending goid", func() {
			gs := packGoroutines(8192)
			// 8000 -> 4000 -> 2000 -> 1000 -> ... -> 1 by the i/2 parent rule.
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: gs, Current: 8000,
			}, false)

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
			}, false)

			Expect(out.Threads[0].MID).To(Equal(63))
			Expect(out.Threads[0].Current).To(BeTrue())
		})

		It("produces byte-identical output for the same input", func() {
			snap := protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192), Threads: packThreads(64), Current: 4096,
			}
			first, firstReport := protocol.PackSnapshot(snap, false)
			second, secondReport := protocol.PackSnapshot(snap, false)

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
			}, false)

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
				}, false)

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
			}, false)

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
			}, false)

			Expect(out.Threads).To(HaveLen(300))
		})
	})

	Describe("totals", func() {
		It("is absent on a complete unclipped snapshot", func() {
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(10), Threads: packThreads(4), Current: 1,
			}, false)

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
			}, false)

			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Goroutines).To(Equal(8192))
			Expect(out.Totals.Threads).To(Equal(64))
			Expect(out.Totals.Clipped).To(BeFalse())
			Expect(report.Omitted()).To(BeTrue())
		})

		It("is present with original counts when the goroutine list omits elements", func() {
			out, _ := protocol.PackGoroutines(packGoroutines(8192), false)

			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Goroutines).To(Equal(8192))
			Expect(out.Totals.Threads).To(BeZero(), "this shape carries no threads")
		})

		It("is present on a complete snapshot when the scan was clipped", func() {
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(10), Threads: packThreads(4), Current: 1,
			}, true)

			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Clipped).To(BeTrue())
			Expect(out.Totals.Goroutines).To(Equal(10), "a lower bound, flagged as such")
		})

		It("is present on a complete goroutine list when the scan was clipped", func() {
			out, _ := protocol.PackGoroutines(packGoroutines(10), true)

			Expect(out.Totals).NotTo(BeNil())
			Expect(out.Totals.Clipped).To(BeTrue())
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
			}, false)

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
			}, false)

			Expect(report.Degraded).To(BeTrue())
			Expect(report.Oversized).To(BeFalse())
			Expect(out.Created).To(HaveLen(2000))
		})

		It("packs the largest delta the runtime scan can produce", func() {
			// created/exited are bounded by the debugger's goroutine scan
			// ceiling, so the packer must still conform at that size.
			created := make([]int, 8192)
			exited := make([]int, 8192)
			for i := range created {
				created[i] = math.MaxInt64 - i
				exited[i] = math.MaxInt64 - 10000 - i
			}
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192),
				Threads:    packThreads(64),
				Current:    1,
				Created:    created,
				Exited:     exited,
			}, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(report.Oversized).To(BeFalse())
			Expect(out.Created).To(HaveLen(8192))
			Expect(out.Exited).To(HaveLen(8192))
			Expect(out.Goroutines).NotTo(BeEmpty())
			Expect(eventBytes(protocol.EventGoroutineSnapshot, out)).
				To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
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
			}, false)

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
			}, false)
			elements, envelopes := protocol.PackMarshalCounts()

			Expect(elements).To(BeNumerically("<=", 2*(len(gs)+len(ts))))
			Expect(envelopes).To(BeNumerically("<=", 4),
				"a per-candidate whole-payload marshal would be O(n^2)")
		})

		It("scales linearly rather than quadratically", func() {
			protocol.ResetPackMarshalCounts()
			protocol.PackGoroutines(packGoroutines(1000), false)
			small, _ := protocol.PackMarshalCounts()

			protocol.ResetPackMarshalCounts()
			protocol.PackGoroutines(packGoroutines(4000), false)
			large, _ := protocol.PackMarshalCounts()

			Expect(large).To(BeNumerically("<=", 5*small))
		})
	})

	Describe("empty and degenerate inputs", func() {
		It("packs an empty snapshot without degrading", func() {
			out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{}, false)

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
			}, false)

			Expect(report.Degraded).To(BeFalse())
			Expect(out.Goroutines).To(Equal(synthetic))
			Expect(out.Totals).To(BeNil())
		})
	})

	Describe("round trip", func() {
		It("survives the wire with totals", func() {
			out, _ := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
				Goroutines: packGoroutines(8192), Threads: packThreads(64), Current: 1,
			}, true)

			evt := protocol.MustEvent(protocol.EventGoroutineSnapshot, 7, out)
			raw, err := protocol.MarshalEvent(evt)
			Expect(err).NotTo(HaveOccurred())
			decodedEvent, err := protocol.UnmarshalEvent(raw)
			Expect(err).NotTo(HaveOccurred())

			var decoded protocol.GoroutineSnapshotPayload
			Expect(protocol.DecodeEventPayload(decodedEvent, &decoded)).To(Succeed())
			Expect(decoded.Totals).NotTo(BeNil())
			Expect(decoded.Totals.Goroutines).To(Equal(8192))
			Expect(decoded.Totals.Clipped).To(BeTrue())
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

func withCurrent(gs []protocol.Goroutine, id int) []protocol.Goroutine {
	out := make([]protocol.Goroutine, len(gs))
	copy(out, gs)
	for i := range out {
		out[i].Current = out[i].ID == id
	}
	return out
}
