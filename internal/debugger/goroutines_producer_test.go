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
	img := &runtimeImage{fb: fb, off: off, allgs: allgs, allm: allm}
	// Seeding one header must never disturb the other. Their real addresses come
	// from the image, whose global layout differs between ELF and Mach-O, so this
	// is asserted rather than assumed — an overlap would corrupt a spec in a way
	// that looks like a debugger bug.
	Expect(allgs+24 <= allm || allm+8 <= allgs).To(BeTrue(),
		"runtime.allgs (0x%x) and runtime.allm (0x%x) overlap", allgs, allm)
	// Start from an explicitly empty runtime rather than from whatever unseeded
	// memory happens to read as: "nobody wrote here" is not the same claim as
	// "this reads as empty", and only the second one is what a spec relies on.
	img.clearGoroutines()
	img.clearThreads()
	return img
}

// clearGoroutines makes the runtime.allgs slice header read as absent.
func (r *runtimeImage) clearGoroutines() {
	r.u64(r.allgs, 0)
	r.u64(r.allgs+8, 0)
	r.u64(r.allgs+16, 0)
}

func (r *runtimeImage) clearThreads() { r.u64(r.allm, 0) }

// unreadableRuntime makes every read of the goroutine table fail outright, which
// is what a stripped binary, an attach without DWARF, or a pre-init entry stop
// look like to the reader. It is expressed as a read failure rather than as
// zeroed memory because "this address reads as empty" depends on the image's
// global layout, and that differs between ELF and Mach-O — a spec that means
// "the runtime cannot be read" should not be able to pass on one platform and
// fail on the other.
func (r *runtimeImage) unreadableRuntime(unreadable bool) {
	if !unreadable {
		r.fb.failReadFrom, r.fb.failReadTo = 0, 0
		return
	}
	r.fb.failReadFrom, r.fb.failReadTo = r.allgs, r.allgs+24
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
			img.seedGoroutines(64, uint64(debugger.ExportedMaxGoroutineScan())+1, 3)
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

		It("flags the thread scan when the M list outruns its ceiling", func() {
			img.seedGoroutines(8, 8, 3)
			img.seedThreads(debugger.ExportedMaxThreadScan() + 16)

			snap, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Totals).NotTo(BeNil())
			Expect(snap.Totals.ThreadsClipped).To(BeTrue())
			Expect(snap.Totals.GoroutinesClipped).To(BeFalse(),
				"the goroutine walk finished, so calling its count approximate would be a lie")
			Expect(snap.Totals.Threads).To(BeNumerically("<=", debugger.ExportedMaxThreadScan()))
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
			first := debugger.ExportedGoroutineSnapshot(d)
			Expect(first.Created).To(BeEmpty(), "the first snapshot has no baseline to diff")
			Expect(first.Exited).To(BeEmpty())

			img.seedGoroutines(protocol.MaxSnapshotGoroutines+8,
				uint64(protocol.MaxSnapshotGoroutines+8), 3)
			second := debugger.ExportedGoroutineSnapshot(d)
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
			Expect(debugger.ExportedMaxGoroutineScan()).To(BeNumerically("<=", protocol.MaxLifecycleDeltaIDs))
		})
	})

	Describe("degraded snapshots", func() {
		It("falls back to the synthetic goroutine when the runtime is unreadable", func() {
			// An unreadable runtime.allgs: the pre-init entry stop, a stripped
			// binary, or attach-without-DWARF.
			img.seedThreads(4)
			img.unreadableRuntime(true)

			snap, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			// The composed contract. The stop is still reported as the legacy
			// single synthetic goroutine rather than erroring, AND the counts
			// that stand for a runtime nobody could read are marked as the floor
			// they are: absent totals mean "complete and exact" on the wire, so
			// emitting none here would claim the runtime holds exactly one
			// goroutine and no threads. "At least one" is the honest reading
			// whether the runtime is merely not up yet or a scan failed partway.
			Expect(snap.Goroutines).To(HaveLen(1))
			Expect(snap.Goroutines[0].ID).To(BeZero())
			Expect(snap.Goroutines[0].Status).To(Equal("unknown"))
			Expect(snap.Totals).NotTo(BeNil(),
				"a degraded read must not present itself as a complete, exact count")
			Expect(snap.Totals.Goroutines).To(Equal(1))
			Expect(snap.Totals.Threads).To(Equal(0))
			Expect(snap.Totals.GoroutinesClipped).To(BeTrue())
			Expect(snap.Totals.ThreadsClipped).To(BeTrue())
			Expect(snap.Created).To(BeEmpty(), "a degraded read invents no lifecycle events")
			Expect(snap.Exited).To(BeEmpty())
		})

		It("marks the degraded goroutine LIST as a lower bound too", func() {
			// The snapshot is not the only degraded shape on the wire: the
			// Goroutines query answers a DAP threads request from the same
			// synthetic stand-in, and absent totals there make the same claim of
			// an exact count. Both degraded paths must read as a floor, or the
			// honesty depends on which request the client happened to send.
			img.seedThreads(4)
			img.unreadableRuntime(true)

			payload, err := d.Goroutines()
			Expect(err).NotTo(HaveOccurred())
			Expect(payload.Goroutines).To(HaveLen(1))
			Expect(payload.Goroutines[0].ID).To(BeZero())
			Expect(payload.Totals).NotTo(BeNil(),
				"a degraded list must not present itself as a complete, exact count")
			Expect(payload.Totals.Goroutines).To(Equal(1))
			Expect(payload.Totals.GoroutinesClipped).To(BeTrue())
		})

		It("does not let an unreadable runtime look like every goroutine exited", func() {
			img.seedGoroutines(8, 8, 3)
			img.seedThreads(4)
			_, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())

			// Make the goroutine table unreadable so the next snapshot degrades.
			img.unreadableRuntime(true)
			degraded, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(degraded.Exited).To(BeEmpty())

			// The baseline must be untouched, so restoring the runtime reports
			// no spurious creations either.
			img.unreadableRuntime(false)
			restored, err := d.GoroutineSnapshot()
			Expect(err).NotTo(HaveOccurred())
			Expect(restored.Created).To(BeEmpty())
			Expect(restored.Exited).To(BeEmpty())
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
