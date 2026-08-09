//go:build e2e && linux && amd64

// Minimal reproduction of #198, distilled from the full `overlap` spec.
//
// Two OS threads, each pinned with runtime.LockOSThread, each hammering its own
// breakpoint. On linux, ptrace stops are per-thread and bingo does not stop the
// world, so while the engine steps one thread off its disarmed trap the other is
// already sitting in an unreaped INT3 stop that Wait4(-1, WALL) can consume.
//
// The invariant, and the whole assertion: at every stop, BOTH breakpoints are
// still installed. SetBreakpoint must therefore fail with errBreakpointExists —
// breakpointTable.set consults byAddr BEFORE it reads or writes tracee memory,
// so a success means the entry is gone and cannot be an artifact of the probe.
//
// The target parks both threads on a `start` gate and publishes `ready` only
// once they are both up: a thread that clones while the engine is suspended
// stops at PTRACE_EVENT_CLONE with no waitLoop to reap it and would freeze.
//
// The full `overlap` spec adds the /proc + DWARF non-vacuity evidence and the
// same-address (two threads on ONE breakpoint) variant; this file is the
// smallest thing that still fails.

package integration

import (
	"os"
	"path/filepath"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// Each marker's breakpoint line is the SECOND statement: Go 1.25.5 folds a
// function's first statement into the prologue's func-decl line, leaving it
// is_stmt=false and unresolvable by dwarfReader.PCForFileLine.
const overlapMinTargetSrc = `package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var sink int64

//go:noinline
func markerA(i int) int {
	v := i * 2
	atomic.AddInt64(&sink, int64(v)) // MARK_A
	return v
}

//go:noinline
func markerB(i int) int {
	v := i * 3
	atomic.AddInt64(&sink, int64(v)) // MARK_B
	return v
}

func main() {
	dir := os.Args[1]
	iters, _ := strconv.Atoi(os.Args[2])
	go func() { time.Sleep(180 * time.Second); os.Exit(0) }()

	start := filepath.Join(dir, "start")
	waitFile := func(p string) {
		for {
			if _, err := os.Stat(p); err == nil {
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
	}

	var wg sync.WaitGroup
	var up int32
	run := func(marker func(int) int) {
		defer wg.Done()
		runtime.LockOSThread()
		atomic.AddInt32(&up, 1)
		waitFile(start)
		for i := 0; i < iters; i++ {
			marker(i)
		}
	}
	wg.Add(2)
	go run(markerA)
	go run(markerB)

	for atomic.LoadInt32(&up) < 2 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(dir, "ready"), []byte("x"), 0o644)

	wg.Wait()
	os.Exit(0)
}
`

var _ = Describe("Linux amd64 step-over vs sibling breakpoint (minimal)", Label("linux"), func() {
	It("keeps every breakpoint armed across a step-over", Label("overlapmin"), func() {
		const name = "overlapmin_target"
		src := name + ".go"
		lineA := markerLine(overlapMinTargetSrc, "// MARK_A")
		lineB := markerLine(overlapMinTargetSrc, "// MARK_B")

		dir := GinkgoT().TempDir()
		bin := buildTarget(name, overlapMinTargetSrc)
		iters := envInt("BINGO_E2E_OVERLAPMIN_ITERS", 40)

		d := debugger.New(nil)
		Expect(d.Launch(bin, []string{dir, strconv.Itoa(iters)}, nil)).To(Succeed(), "Launch")
		DeferCleanup(func() {
			done := make(chan struct{})
			go func() { _ = d.Kill(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
			}
		})
		awaitEvent(d.Events(), 20*time.Second, protocol.EventStepped) // entry stop

		for _, line := range []int{lineA, lineB} {
			_, err := d.SetBreakpoint(src, line)
			Expect(err).NotTo(HaveOccurred(), "SetBreakpoint %s:%d", src, line)
		}

		// Resume so the runtime can create both marker threads: clone events are
		// only absorbed by Backend.Wait, which runs only while a waitLoop is in
		// flight. Then release them together, while the tracee is still running.
		Expect(d.Continue()).To(Succeed(), "Continue to spin up marker threads")
		Eventually(func() bool {
			_, err := os.Stat(filepath.Join(dir, "ready"))
			return err == nil
		}, 40*time.Second, 5*time.Millisecond).Should(BeTrue(), "both marker threads parked")
		Expect(os.WriteFile(filepath.Join(dir, "start"), []byte("x"), 0o600)).To(Succeed())

		stops := 0
		for {
			ev := awaitEvent(d.Events(), 30*time.Second,
				protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
			if ev.Kind == protocol.EventProcessExited {
				break
			}
			Expect(ev.Kind).To(Equal(protocol.EventBreakpointHit),
				"stop %d: unexpected %s: %s", stops, ev.Kind, ev.Payload)
			stops++
			Expect(stops).To(BeNumerically("<=", 2*iters+20), "runaway stop count")

			// THE INVARIANT. The resume that produced this stop stepped one
			// breakpoint over; if a sibling's stop was consumed during that
			// step-over, the stepped-over entry was dropped from the table and
			// never reinstalled.
			for _, line := range []int{lineA, lineB} {
				_, err := d.SetBreakpoint(src, line)
				Expect(err).To(HaveOccurred(),
					"breakpoint %s:%d was silently orphaned by a sibling stop during a step-over "+
						"(stop %d): SetBreakpoint succeeded, so the entry is gone from "+
						"breakpointTable.byAddr — it is disarmed in tracee memory and can never "+
						"fire or be cleared again", src, line, stops)
				Expect(err.Error()).To(ContainSubstring("already installed"),
					"probe SetBreakpoint(%s:%d) returned an unexpected error", src, line)
			}

			Expect(d.Continue()).To(Succeed(), "Continue after stop %d", stops)
		}

		Expect(stops).To(BeNumerically(">=", 2),
			"the tracee must actually have hit the breakpoints")
		AddReportEntry("overlapmin-stops", stops)
	})
})
