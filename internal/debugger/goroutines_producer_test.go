package debugger_test

import (
	"encoding/binary"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// These specs drive the REAL runtime reader — DWARF-resolved offsets against a
// synthetic runtime.allgs / runtime.allm planted in the fake backend's memory —
// so the producer wiring is exercised end to end rather than mocked. They are
// what proves the goroutine event contract is actually in force at the source:
// that every produced event is bounded, that each collection's scan-clipped
// flag is independent and truthful, and that lifecycle accounting still runs
// against the full runtime rather than the trimmed frame. See issue #194.

// runtimeImage lays out a synthetic Go runtime in the fake backend's address
// space. Addresses are arbitrary but disjoint; sizes are generous so adjacent
// structs never overlap regardless of the host Go version's real struct sizes.
const (
	gArrayBase    = uint64(0x1000_0000) // the []*g backing array
	gStructBase   = uint64(0x2000_0000) // fabricated g structs
	gStructSpan   = uint64(0x400)
	mStructBase   = uint64(0x4000_0000) // fabricated m structs
	mStructSpan   = uint64(0x400)
	mStructRegion = uint64(0x1000_0000) // the whole span reserved for m structs

	// Stack window of the goroutine the tracee is stopped in. The reader
	// identifies the current goroutine by SP containment, so only this one
	// brackets the seeded SP.
	currentStackLo = uint64(0x7000_0000)
	currentStackHi = uint64(0x7000_1000)
	currentSP      = uint64(0x7000_0800)

	gStatusRunning = 2 // _Grunning
)

type runtimeImage struct {
	fb      *fakeBackend
	off     map[string]int64
	allgs   uint64
	allm    uint64
	current int
}

func newRuntimeImage(fb *fakeBackend, d debugger.Debugger) *runtimeImage {
	off, ok := debugger.ExportedGoOffsets(d)
	Expect(ok).To(BeTrue(), "the fixture binary must carry runtime DWARF")
	allgs, ok := debugger.ExportedRuntimeVarAddr(d, "runtime.allgs")
	Expect(ok).To(BeTrue())
	allm, ok := debugger.ExportedRuntimeVarAddr(d, "runtime.allm")
	Expect(ok).To(BeTrue())
	return &runtimeImage{fb: fb, off: off, allgs: allgs, allm: allm}
}

func (r *runtimeImage) u64(addr uint64, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	r.fb.seedMem(addr, b[:])
}

func (r *runtimeImage) u32(addr uint64, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	r.fb.seedMem(addr, b[:])
}

func gAddr(i int) uint64 { return gStructBase + uint64(i)*gStructSpan }

// seedGoroutines plants n goroutines with goids 1..n. sliceLen is what the
// slice header advertises, which is how a test drives the reader past its scan
// ceiling without allocating that many structs. current names the goroutine
// whose stack brackets the seeded SP.
func (r *runtimeImage) seedGoroutines(n int, sliceLen uint64, current int) {
	r.current = current
	r.u64(r.allgs, gArrayBase)
	r.u64(r.allgs+8, sliceLen)
	r.u64(r.allgs+16, sliceLen) // cap

	for i := 0; i < n; i++ {
		goid := i + 1
		g := gAddr(i)
		r.u64(gArrayBase+uint64(i)*8, g)
		r.u64(g+uint64(r.off["g.goid"]), uint64(goid))
		r.u32(g+uint64(r.off["g.atomicstatus"]), gStatusRunning)
		// Every goroutine but the root is spawned by goid 1, so the current
		// goroutine's ancestor chain is short and the tree is well formed.
		if goid != 1 {
			r.u64(g+uint64(r.off["g.parentGoid"]), 1)
		}
		if goid == current {
			r.u64(g+uint64(r.off["g.stack"])+uint64(r.off["stack.lo"]), currentStackLo)
			r.u64(g+uint64(r.off["g.stack"])+uint64(r.off["stack.hi"]), currentStackHi)
			continue
		}
		// A disjoint, non-containing stack window keeps exactly one goroutine
		// current.
		lo := uint64(0x8000_0000) + uint64(i)*0x2000
		r.u64(g+uint64(r.off["g.stack"])+uint64(r.off["stack.lo"]), lo)
		r.u64(g+uint64(r.off["g.stack"])+uint64(r.off["stack.hi"]), lo+0x1000)
	}
}

// seedThreads plants a terminated runtime.allm linked list of n Ms.
func (r *runtimeImage) seedThreads(n int) {
	if n == 0 {
		r.u64(r.allm, 0)
		return
	}
	r.u64(r.allm, mStructBase)
	for i := 0; i < n; i++ {
		m := mStructBase + uint64(i)*mStructSpan
		r.u64(m+uint64(r.off["m.procid"]), uint64(1000+i))
		r.u64(m+uint64(r.off["m.id"]), uint64(i))
		// The first M runs the current goroutine, which is what makes it the
		// current thread and a required packing anchor.
		if i == 0 && r.current != 0 {
			r.u64(m+uint64(r.off["m.curg"]), gAddr(r.current-1))
		}
		next := uint64(0)
		if i+1 < n {
			next = mStructBase + uint64(i+1)*mStructSpan
		}
		r.u64(m+uint64(r.off["m.alllink"]), next)
	}
}

// truncateThreadWalkAfter makes every read at or beyond the nth M fail, so the
// walk ends with the list still going — the reader's other "stopped early"
// branch, distinct from hitting its ceiling.
func (r *runtimeImage) truncateThreadWalkAfter(n int) {
	r.fb.failReadFrom = mStructBase + uint64(n)*mStructSpan
	r.fb.failReadTo = mStructBase + mStructRegion
}

var _ = Describe("goroutine event production", func() {
	var (
		fb  *fakeBackend
		d   debugger.Debugger
		img *runtimeImage
	)

	BeforeEach(func() {
		bin, err := inspectFixture()
		Expect(err).NotTo(HaveOccurred())

		fb = newFakeBackend()
		d = debugger.NewWithBackend(fb, nil)
		debugger.ExportedLoadDWARF(d, bin)
		fb.seedRegs(debugger.Registers{SP: currentSP, PC: 0x1234})
		debugger.ExportedForceSuspended(d)
		img = newRuntimeImage(fb, d)
	})

	AfterEach(func() {
		_ = d.Kill()
		if !fb.stopped {
			close(fb.stopCh)
			fb.stopped = true
		}
	})

	Describe("scan clipping", func() {
		It("reports neither collection clipped on a walk that completes", func() {
			img.seedGoroutines(8, 8, 3)
			img.seedThreads(4)

			snap, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Goroutines).To(HaveLen(8))
			Expect(snap.Threads).To(HaveLen(4))
			Expect(snap.Current).To(Equal(3))
			Expect(snap.Totals).To(BeNil(),
				"a complete result must stay byte-identical to the unbounded shape")
		})

		It("flags only the goroutine scan when runtime.allgs outruns its ceiling", func() {
			// The slice claims more goroutines than the reader will walk; only
			// the ones it can reach are backed by real structs.
			img.seedGoroutines(64, debugger.ExportedMaxGoroutineScan+1, 3)
			img.seedThreads(4)

			snap, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Totals).NotTo(BeNil(),
				"a clipped scan must say so even when nothing was trimmed for size")
			Expect(snap.Totals.GoroutinesClipped).To(BeTrue())
			Expect(snap.Totals.ThreadsClipped).To(BeFalse(),
				"the thread walk finished, so calling its count approximate would be a lie")
			Expect(snap.Totals.Goroutines).To(Equal(len(snap.Goroutines)),
				"totals report what was scanned, not what the ceiling was")
		})

		It("flags only the thread scan when the M list becomes unreadable", func() {
			img.seedGoroutines(8, 8, 3)
			img.seedThreads(6)
			img.truncateThreadWalkAfter(3)

			snap, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(len(snap.Threads)).To(BeNumerically("<", 6),
				"the walk must not reach every M once the list becomes unreadable")
			Expect(snap.Totals).NotTo(BeNil())
			Expect(snap.Totals.ThreadsClipped).To(BeTrue(),
				"a walk that ended on an unreadable link left threads unvisited")
			Expect(snap.Totals.GoroutinesClipped).To(BeFalse(),
				"the goroutine walk finished, so calling its count approximate would be a lie")
		})

		It("flags the thread scan when the M list outruns its ceiling", func() {
			img.seedGoroutines(8, 8, 3)
			img.seedThreads(debugger.ExportedMaxThreadScan + 16)

			snap, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Totals).NotTo(BeNil())
			Expect(snap.Totals.ThreadsClipped).To(BeTrue())
			Expect(snap.Totals.Threads).To(BeNumerically("<=", debugger.ExportedMaxThreadScan))
		})
	})

	Describe("bounding", func() {
		It("keeps a saturating snapshot inside the wire contract", func() {
			// More goroutines than the element cap admits, so the packer must
			// trim — and the event must still fit both ceilings.
			img.seedGoroutines(protocol.MaxSnapshotGoroutines+500,
				uint64(protocol.MaxSnapshotGoroutines+500), 3)
			img.seedThreads(64)

			snap, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(len(snap.Goroutines)).To(BeNumerically("<=", protocol.MaxSnapshotGoroutines))
			Expect(snap.Totals).NotTo(BeNil())
			Expect(snap.Totals.Goroutines).To(Equal(protocol.MaxSnapshotGoroutines + 500))
			Expect(snap.Totals.GoroutinesClipped).To(BeFalse(),
				"the walk completed; only the frame was trimmed")

			evt := protocol.MustEvent(protocol.EventGoroutineSnapshot, 1, snap)
			raw, err := protocol.MarshalEvent(evt)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(raw)).To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))

			Expect(snap.Current).To(Equal(3), "the current goroutine is a required anchor")
			Expect(goidsOf(snap.Goroutines)).To(ContainElement(3))
			Expect(goidsOf(snap.Goroutines)).To(ContainElement(1),
				"the current goroutine's whole spawn chain must survive")
		})

		It("keeps a saturating goroutine list inside the wire contract", func() {
			img.seedGoroutines(protocol.MaxSnapshotGoroutines+500,
				uint64(protocol.MaxSnapshotGoroutines+500), 3)
			img.seedThreads(4)

			payload, err := d.Goroutines()
			Expect(err).NotTo(HaveOccurred())
			Expect(len(payload.Goroutines)).To(BeNumerically("<=", protocol.MaxSnapshotGoroutines))
			Expect(payload.Totals).NotTo(BeNil())
			Expect(payload.Totals.Goroutines).To(Equal(protocol.MaxSnapshotGoroutines + 500))
			Expect(payload.Goroutines).NotTo(BeEmpty(),
				"a flat list must degrade by truncation, never to empty — an empty "+
					"threads response makes the DAP translator invent a synthetic main")

			evt := protocol.MustEvent(protocol.EventGoroutines, 1, payload)
			raw, err := protocol.MarshalEvent(evt)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(raw)).To(BeNumerically("<=", protocol.MaxGoroutineEventBytes))
		})
	})

	Describe("lifecycle accounting", func() {
		It("diffs the runtime, not the trimmed frame", func() {
			// A first snapshot seeds the baseline; the second adds goroutines
			// beyond the element cap. If the deltas were computed against the
			// packed subset, every trimmed goroutine would look like it exited.
			img.seedGoroutines(protocol.MaxSnapshotGoroutines,
				uint64(protocol.MaxSnapshotGoroutines), 3)
			img.seedThreads(4)
			first, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(first.Created).To(BeEmpty(), "the first snapshot has no baseline to diff")
			Expect(first.Exited).To(BeEmpty())

			img.seedGoroutines(protocol.MaxSnapshotGoroutines+8,
				uint64(protocol.MaxSnapshotGoroutines+8), 3)
			second, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(second.Created).To(HaveLen(8),
				"exactly the eight new goroutines, regardless of what the frame carried")
			Expect(second.Exited).To(BeEmpty(),
				"trimming a goroutine off the wire must never look like it exited")
		})

		It("cannot produce deltas that overflow the contract", func() {
			// The reason the non-conforming path is unreachable in production:
			// both deltas are set differences over live sets the reader itself
			// caps, so neither can exceed the delta ceiling. If a future change
			// raises the scan without raising the ceiling, this fails here
			// rather than as a dead observer in the field.
			Expect(debugger.ExportedMaxGoroutineScan).To(BeNumerically("<=", protocol.MaxLifecycleDeltaIDs))
		})
	})

	Describe("degraded snapshots", func() {
		It("falls back to the synthetic goroutine when the runtime is unreadable", func() {
			// runtime.allgs left at zero: the pre-init entry stop, a stripped
			// binary, or attach-without-DWARF.
			img.seedThreads(4)

			snap, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Goroutines).To(HaveLen(1))
			Expect(snap.Goroutines[0].ID).To(Equal(1))
			Expect(snap.Goroutines[0].Status).To(Equal("waiting"))
			Expect(snap.Totals).To(BeNil(), "a degraded read omits nothing and clips nothing")
		})

		It("does not let an unreadable runtime look like every goroutine exited", func() {
			img.seedGoroutines(8, 8, 3)
			img.seedThreads(4)
			_, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())

			// Wipe the slice header so the next read degrades.
			img.u64(img.allgs, 0)
			degraded, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(degraded.Exited).To(BeEmpty())

			// The baseline must be untouched, so restoring the runtime reports
			// no spurious creations either.
			img.u64(img.allgs, gArrayBase)
			restored, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(restored.Created).To(BeEmpty())
			Expect(restored.Exited).To(BeEmpty())
		})
	})

	Describe("non-conforming results", func() {
		It("drops a streamed snapshot rather than broadcasting a known violation", func() {
			events := d.Events()
			debugger.ExportedEmitSnapshot(d,
				protocol.GoroutineSnapshotPayload{Goroutines: []protocol.Goroutine{{ID: 1}}},
				protocol.GoroutinePackReport{Oversized: true})
			Consistently(events, "50ms").ShouldNot(Receive(),
				"a conforming consumer treats a contract violation as deterministic and "+
					"stops observing, so emitting one is worse than dropping it")

			debugger.ExportedEmitSnapshot(d,
				protocol.GoroutineSnapshotPayload{Goroutines: []protocol.Goroutine{{ID: 1}}},
				protocol.GoroutinePackReport{})
			Eventually(events).Should(Receive())
		})
	})
})

func goidsOf(gs []protocol.Goroutine) []int {
	out := make([]int, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.ID)
	}
	return out
}
