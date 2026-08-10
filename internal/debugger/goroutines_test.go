package debugger_test

import (
	"encoding/binary"
	"runtime"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

const (
	allgsArrayAddr = uint64(0x900000)
	gBaseAddr      = uint64(0x910000)
	mBaseAddr      = uint64(0xa10000)
	runtimeStride  = uint64(0x1000)
)

type goroutineMemoryFixture struct {
	layout debugger.ExportedGoRuntimeLayout
	g      []uint64
	m      []uint64
}

func seedU32(fb *fakeBackend, addr uint64, value uint32) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], value)
	fb.seedMem(addr, buf[:])
}

func seedU64(fb *fakeBackend, addr, value uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], value)
	fb.seedMem(addr, buf[:])
}

func seedGoroutineMemory(fb *fakeBackend, d debugger.Debugger) goroutineMemoryFixture {
	layout := debugger.ExportedGoRuntimeLayoutFor(d)
	fixture := goroutineMemoryFixture{
		layout: layout,
		g: []uint64{
			gBaseAddr,
			gBaseAddr + runtimeStride,
			gBaseAddr + 2*runtimeStride,
			gBaseAddr + 3*runtimeStride,
			gBaseAddr + 4*runtimeStride,
		},
		m: []uint64{
			mBaseAddr,
			mBaseAddr + runtimeStride,
		},
	}

	slots := []uint64{
		fixture.g[0],
		fixture.g[1],
		fixture.g[2],
		0,
		fixture.g[3],
		fixture.g[4],
	}
	seedU64(fb, layout.Allgs, allgsArrayAddr)
	seedU64(fb, layout.Allgs+8, uint64(len(slots)))
	for i, gptr := range slots {
		seedU64(fb, allgsArrayAddr+uint64(i)*8, gptr)
	}

	for i := 0; i < 3; i++ {
		seedU32(fb, fixture.g[i]+layout.GAtomicstatus, 4)
		seedU64(fb, fixture.g[i]+layout.GGoid, uint64(101+i))
		seedU64(fb, fixture.g[i]+layout.GStack+layout.StackLo, 0x7000+uint64(i)*0x1000)
		seedU64(fb, fixture.g[i]+layout.GStack+layout.StackHi, 0x8000+uint64(i)*0x1000)
	}
	seedU32(fb, fixture.g[3]+layout.GAtomicstatus, 6)
	seedU64(fb, fixture.g[3]+layout.GGoid, 104)
	seedU32(fb, fixture.g[4]+layout.GAtomicstatus, 4)
	seedU64(fb, fixture.g[4]+layout.GGoid, 0)

	seedU64(fb, layout.Allm, fixture.m[0])
	seedU64(fb, fixture.m[0]+layout.MProcid, 11)
	seedU64(fb, fixture.m[0]+layout.MCurg, fixture.g[0])
	seedU64(fb, fixture.m[0]+layout.MAlllink, fixture.m[1])
	seedU64(fb, fixture.m[1]+layout.MProcid, 22)
	seedU64(fb, fixture.m[1]+layout.MCurg, fixture.g[1])
	seedU64(fb, fixture.m[1]+layout.MAlllink, 0)
	seedU64(fb, fixture.g[0]+layout.GM, fixture.m[0])
	seedU64(fb, fixture.g[1]+layout.GM, fixture.m[1])
	fb.seedRegs(debugger.Registers{SP: 0x8800})

	return fixture
}

func goroutineIDs(gs []protocol.Goroutine) []int {
	ids := make([]int, len(gs))
	for i, g := range gs {
		ids[i] = g.ID
	}
	return ids
}

type snapshotObservation struct {
	Goroutines []int
	Threads    int
	Current    int
	Selected   int
	Created    []int
	Exited     []int
}

func observeSnapshot(snap protocol.GoroutineSnapshotPayload) snapshotObservation {
	return snapshotObservation{
		Goroutines: goroutineIDs(snap.Goroutines),
		Threads:    len(snap.Threads),
		Current:    snap.Current,
		Selected:   debugger.ExportedCurrentGoroutineFrom(snap).ID,
		Created:    snap.Created,
		Exited:     snap.Exited,
	}
}

func expectDegradedSnapshotPreservesBaseline(degraded, recovered protocol.GoroutineSnapshotPayload) {
	Expect([]snapshotObservation{
		observeSnapshot(degraded),
		observeSnapshot(recovered),
	}).To(Equal([]snapshotObservation{
		{Goroutines: []int{0}},
		{
			Goroutines: []int{101, 102},
			Threads:    2,
			Current:    102,
			Selected:   102,
			Exited:     []int{103},
		},
	}))
}

var _ = Describe("goroutine snapshot partial reads", func() {
	var (
		fb      *fakeBackend
		d       debugger.Debugger
		fixture goroutineMemoryFixture
	)

	BeforeEach(func() {
		bin, err := inspectFixture()
		Expect(err).NotTo(HaveOccurred())

		fb = newFakeBackend()
		d = debugger.NewWithBackend(fb, nil)
		debugger.ExportedLoadDWARF(d, bin)
		debugger.ExportedForceSuspended(d)
		fixture = seedGoroutineMemory(fb, d)

		baseline := debugger.ExportedGoroutineSnapshot(d)
		Expect(observeSnapshot(baseline)).To(Equal(snapshotObservation{
			Goroutines: []int{101, 102, 103},
			Threads:    2,
			Current:    102,
			Selected:   102,
		}))
	})

	AfterEach(func() {
		_ = d.Kill()
		if !fb.stopped {
			close(fb.stopCh)
			fb.stopped = true
		}
	})

	DescribeTable("degrades the whole snapshot and preserves its lifecycle baseline",
		func(faultAddr func() uint64) {
			seedU32(fb, fixture.g[2]+fixture.layout.GAtomicstatus, 6)
			fb.failNextReadAt(faultAddr())

			degraded := debugger.ExportedGoroutineSnapshot(d)
			recovered := debugger.ExportedGoroutineSnapshot(d)

			expectDegradedSnapshotPreservesBaseline(degraded, recovered)
		},
		Entry("when an allgs slot pointer is unreadable", func() uint64 {
			return allgsArrayAddr + 8
		}),
		Entry("when a required g status is unreadable", func() uint64 {
			return fixture.g[1] + fixture.layout.GAtomicstatus
		}),
		Entry("when a required g goid is unreadable", func() uint64 {
			return fixture.g[1] + fixture.layout.GGoid
		}),
		Entry("when a non-current g stack lower bound is unreadable", func() uint64 {
			return fixture.g[0] + fixture.layout.GStack + fixture.layout.StackLo
		}),
		Entry("when the current g stack upper bound is unreadable", func() uint64 {
			return fixture.g[1] + fixture.layout.GStack + fixture.layout.StackHi
		}),
		Entry("when the allm head pointer is unreadable", func() uint64 {
			return fixture.layout.Allm
		}),
		Entry("when an allm link is unreadable", func() uint64 {
			return fixture.m[0] + fixture.layout.MAlllink
		}),
	)

	DescribeTable("keeps degraded on-demand snapshots read-only",
		func(faultAddr func() uint64) {
			seedU32(fb, fixture.g[2]+fixture.layout.GAtomicstatus, 6)
			fb.failNextReadAt(faultAddr())

			degraded := debugger.ExportedGoroutineSnapshotQuery(d)
			recovered := debugger.ExportedGoroutineSnapshot(d)

			expectDegradedSnapshotPreservesBaseline(degraded, recovered)
		},
		Entry("when allgs is incomplete", func() uint64 {
			return allgsArrayAddr + 8
		}),
		Entry("when allm is incomplete", func() uint64 {
			return fixture.m[0] + fixture.layout.MAlllink
		}),
	)

	It("keeps nil, dead, and zero-goid entries as intentional filters", func() {
		seedU32(fb, fixture.g[2]+fixture.layout.GAtomicstatus, 6)
		dead := debugger.ExportedGoroutineSnapshot(d)

		seedU32(fb, fixture.g[2]+fixture.layout.GAtomicstatus, 4)
		liveAgain := debugger.ExportedGoroutineSnapshot(d)

		Expect([]snapshotObservation{
			observeSnapshot(dead),
			observeSnapshot(liveAgain),
		}).To(Equal([]snapshotObservation{
			{
				Goroutines: []int{101, 102},
				Threads:    2,
				Current:    102,
				Selected:   102,
				Exited:     []int{103},
			},
			{
				Goroutines: []int{101, 102, 103},
				Threads:    2,
				Current:    102,
				Selected:   102,
				Created:    []int{103},
			},
		}))
	})

	DescribeTable("keeps optional metadata reads best-effort",
		func(faultAddr func() uint64) {
			fb.failNextReadAt(faultAddr())

			snap := debugger.ExportedGoroutineSnapshot(d)

			Expect(observeSnapshot(snap)).To(Equal(snapshotObservation{
				Goroutines: []int{101, 102, 103},
				Threads:    2,
				Current:    102,
				Selected:   102,
			}))
		},
		Entry("for a goroutine parent", func() uint64 {
			return fixture.g[1] + fixture.layout.GParentGoid
		}),
		Entry("for a thread id", func() uint64 {
			return fixture.m[0] + fixture.layout.MProcid
		}),
	)

	DescribeTable("degrades an on-demand Goroutines read instead of returning a partial set",
		func(faultAddr func() uint64) {
			fb.failNextReadAt(faultAddr())

			gs, err := d.Goroutines()
			Expect(err).NotTo(HaveOccurred())
			Expect(goroutineIDs(gs)).To(Equal([]int{0}))
			Expect(gs[0].Current).To(BeTrue())
		},
		Entry("for an unreadable allgs slot", func() uint64 {
			return allgsArrayAddr + 8
		}),
		Entry("for an unreadable stack lower bound", func() uint64 {
			return fixture.g[0] + fixture.layout.GStack + fixture.layout.StackLo
		}),
		Entry("for an unreadable stack upper bound", func() uint64 {
			return fixture.g[1] + fixture.layout.GStack + fixture.layout.StackHi
		}),
	)

	It("keeps a clipped allm walk distinct from an incomplete one", func() {
		fb.failNextReadAt(fixture.m[0] + fixture.layout.MAlllink)
		incomplete := debugger.ExportedThreadWalkResult(d)
		Expect(incomplete).To(Equal(debugger.ExportedWalkResult{}))

		next := mBaseAddr
		for i := 0; i < debugger.ExportedMaxThreadScan()+1; i++ {
			current := next
			next += runtimeStride
			seedU64(fb, current+fixture.layout.MAlllink, next)
		}

		clipped := debugger.ExportedThreadWalkResult(d)

		Expect(clipped).To(Equal(debugger.ExportedWalkResult{
			Count:    debugger.ExportedMaxThreadScan(),
			Complete: true,
			Clipped:  true,
		}))
	})

	It("keeps a clipped allgs walk distinct from an incomplete one", func() {
		fb.failNextReadAt(allgsArrayAddr)
		incomplete := debugger.ExportedGoroutineWalkResult(d)
		Expect(incomplete).To(Equal(debugger.ExportedWalkResult{}))

		seedU64(fb, fixture.layout.Allgs+8, uint64(debugger.ExportedMaxGoroutineScan()+1))
		for i := 0; i < debugger.ExportedMaxGoroutineScan()+1; i++ {
			seedU64(fb, allgsArrayAddr+uint64(i)*8, fixture.g[0])
		}

		clipped := debugger.ExportedGoroutineWalkResult(d)

		Expect(clipped).To(Equal(debugger.ExportedWalkResult{
			Count:    debugger.ExportedMaxGoroutineScan(),
			Complete: true,
			Clipped:  true,
		}))
	})

	It("preserves lifecycle state when a beyond-cap current anchor read is incomplete", func() {
		const (
			largeAllgsAddr = uint64(0x2000000)
			tailStackLo    = uint64(0xb0000000)
		)
		richCount := debugger.ExportedMaxGoroutineScan()
		tailCurrent := fixture.g[4]

		seedU64(fb, fixture.layout.Allgs, largeAllgsAddr)
		seedU64(fb, fixture.layout.Allgs+8, uint64(richCount+1))
		for i := 0; i < richCount; i++ {
			gptr := fixture.g[i%3]
			seedU64(fb, largeAllgsAddr+uint64(i)*8, gptr)
		}
		seedU32(fb, tailCurrent+fixture.layout.GAtomicstatus, 4)
		seedU64(fb, tailCurrent+fixture.layout.GGoid, 104)
		seedU64(fb, tailCurrent+fixture.layout.GStack+fixture.layout.StackLo, tailStackLo)
		seedU64(fb, tailCurrent+fixture.layout.GStack+fixture.layout.StackHi, tailStackLo+0x1000)
		seedU64(fb, tailCurrent+fixture.layout.GM, fixture.m[1])
		seedU64(fb, fixture.m[1]+fixture.layout.MProcid, 1)
		seedU64(fb, fixture.m[1]+fixture.layout.MCurg, tailCurrent)
		seedU64(fb, largeAllgsAddr+uint64(richCount)*8, tailCurrent)
		fb.seedRegs(debugger.Registers{SP: tailStackLo + 8, TLS: tailCurrent})

		baseline := debugger.ExportedGoroutineSnapshot(d)
		Expect(baseline.Current).To(Equal(104))
		Expect(baseline.Goroutines).To(HaveLen(richCount + 1))
		Expect(baseline.Goroutines[0].ID).To(Equal(104))
		Expect(baseline.Goroutines[0].Current).To(BeTrue())
		Expect(baseline.Created).To(BeNil())
		Expect(baseline.Exited).To(BeNil())
		currentGoroutines := 0
		for _, goroutine := range baseline.Goroutines {
			if goroutine.Current {
				currentGoroutines++
			}
		}
		Expect(currentGoroutines).To(Equal(1))
		currentThreads := 0
		for _, thread := range baseline.Threads {
			if thread.Current {
				currentThreads++
				Expect(thread.GoID).To(Equal(104))
			}
		}
		Expect(currentThreads).To(Equal(1))

		seedU32(fb, fixture.g[2]+fixture.layout.GAtomicstatus, 6)
		// arm64 first rejects X28 as a speculative hint, then reaches the rooted
		// allgs tail. Linux has no register candidate: its exact stopped-M lookup
		// is already rooted, so one unreadable bound must degrade immediately.
		fb.failNextReadAt(tailCurrent + fixture.layout.GStack + fixture.layout.StackLo)
		if runtime.GOARCH == "arm64" {
			fb.failNextReadAt(tailCurrent + fixture.layout.GStack + fixture.layout.StackLo)
		}

		degraded := debugger.ExportedGoroutineSnapshot(d)
		recovered := debugger.ExportedGoroutineSnapshot(d)

		Expect(degraded.Goroutines).To(Equal([]protocol.Goroutine{{
			Status:  "unknown",
			Current: true,
		}}))
		Expect(degraded.Created).To(BeNil())
		Expect(degraded.Exited).To(BeNil())
		Expect(recovered.Current).To(Equal(104))
		Expect(recovered.Created).To(BeNil())
		Expect(recovered.Exited).To(Equal([]int{103}))

		currentThreads = 0
		for _, thread := range recovered.Threads {
			if thread.Current {
				currentThreads++
				Expect(thread.GoID).To(Equal(104))
			}
		}
		Expect(currentThreads).To(Equal(1))
	})

	It("keeps regular step identity synthetic without reading runtime lists", func() {
		seedU64(fb, fixture.m[1]+fixture.layout.MProcid, 1)
		fb.seedRegs(debugger.Registers{
			PC:  0x1234,
			SP:  0x8800,
			TLS: fixture.g[1],
		})
		Expect(d.StepInto()).To(Succeed())
		fb.getRegisterCalls = 0
		fb.readCount = make(map[uint64]int)
		fb.pushStop(debugger.StopEvent{
			Reason: debugger.StopSingleStep,
			TID:    1,
			PC:     0x1234,
		})

		event := mustNextEvent(d)
		Expect(event.Kind).To(Equal(protocol.EventStepped))
		var stepped protocol.SteppedPayload
		Expect(protocol.DecodeEventPayload(event, &stepped)).To(Succeed())
		Expect(stepped.Goroutine.ID).To(BeZero())
		Expect(stepped.Goroutine.Status).To(Equal("unknown"))
		Expect(stepped.Goroutine.Current).To(BeTrue())
		Expect(fb.readCount[fixture.layout.Allgs]).To(BeZero(),
			"regular steps must not read the runtime.allgs root")
		Expect(fb.readCount[fixture.layout.Allm]).To(BeZero(),
			"regular steps must not read the runtime.allm root")
		Expect(fb.readCount[allgsArrayAddr]).To(BeZero(),
			"regular steps must not walk runtime.allgs")
		Expect(fb.readCount[mBaseAddr+fixture.layout.MProcid]).To(BeZero(),
			"regular steps must not walk runtime.allm")
		Expect(fb.getRegisterCalls).To(Equal(1))
	})

	It("retains a complete goroutine set when current identity is unknown", func() {
		fb.seedRegs(debugger.Registers{SP: 0xdeadbeef})

		snap := debugger.ExportedGoroutineSnapshot(d)

		Expect(observeSnapshot(snap)).To(Equal(snapshotObservation{
			Goroutines: []int{101, 102, 103},
			Threads:    2,
		}))
	})

	It("does not substitute the first goroutine when current identity is unknown", func() {
		current := debugger.ExportedCurrentGoroutineFrom(protocol.GoroutineSnapshotPayload{
			Goroutines: []protocol.Goroutine{{ID: 101}, {ID: 102}},
		})

		Expect(current).To(Equal(protocol.Goroutine{
			Status:  "unknown",
			Current: true,
		}))
	})
})
