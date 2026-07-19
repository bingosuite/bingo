package debugger_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// inspectFixtureSrc has two functions with distinctly-named locals so a
// misdirected inspection (reading the wrong thread's frame) is detectable by
// the variable/function names it returns. Built with -N -l so the locals are
// present in DWARF and not optimized away.
const inspectFixtureSrc = `package main

func alpha(x int) int {
	a := x * 2 // alpha-marker
	return a
}

func beta(y int) int {
	b := y + 3 // beta-marker
	return b
}

func main() {
	println(alpha(1) + beta(2))
}
`

var (
	inspectFixtureOnce sync.Once
	inspectFixtureBin  string
	inspectFixtureErr  error
)

// inspectFixture builds the fixture once per test binary and returns its path.
func inspectFixture() (string, error) {
	inspectFixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bingo-inspect-fixture")
		if err != nil {
			inspectFixtureErr = err
			return
		}
		src := filepath.Join(dir, "fix.go")
		if err := os.WriteFile(src, []byte(inspectFixtureSrc), 0o600); err != nil {
			inspectFixtureErr = err
			return
		}
		bin := filepath.Join(dir, "fix")
		cmd := exec.Command("go", "build", "-gcflags=all=-N -l", "-o", bin, src)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
			inspectFixtureErr = fmt.Errorf("build inspect fixture: %v\n%s", buildErr, out)
			return
		}
		inspectFixtureBin = bin
	})
	return inspectFixtureBin, inspectFixtureErr
}

func inspectMarkerLine(marker string) int {
	for i, line := range strings.Split(inspectFixtureSrc, "\n") {
		if strings.Contains(line, marker) {
			return i + 1
		}
	}
	return 0
}

func varNames(vars []protocol.Variable) []string {
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		names = append(names, v.Name)
	}
	return names
}

// Regression coverage for the "inspect the thread the user is stopped on"
// invariant (see engine.activeTID / curTID). Before the fix, Locals and
// Goroutines read threads[0] and StackFrames read the stale lastBPTID, so a
// breakpoint that fired on any thread other than threads[0] — the common case
// on darwin, where threads[0] is frequently an idle runtime M — returned frames
// and variables from an unrelated thread.
var _ = Describe("current-thread inspection", func() {
	var (
		fb       *fakeBackend
		d        debugger.Debugger
		pcAlpha  uint64
		pcBeta   uint64
		fileName = "fix.go"
	)

	BeforeEach(func() {
		bin, err := inspectFixture()
		Expect(err).NotTo(HaveOccurred())

		fb = newFakeBackend()
		d = debugger.NewWithBackend(fb, nil)
		debugger.ExportedLoadDWARF(d, bin)

		pcAlpha, err = debugger.ExportedPCForFileLine(d, fileName, inspectMarkerLine("alpha-marker"))
		Expect(err).NotTo(HaveOccurred())
		pcBeta, err = debugger.ExportedPCForFileLine(d, fileName, inspectMarkerLine("beta-marker"))
		Expect(err).NotTo(HaveOccurred())

		// threads[0]==1 is parked in alpha (stands in for an idle runtime M);
		// the user thread 2 is the one that stops in beta.
		fb.tids = []int{1, 2}
		fb.regs[1] = debugger.Registers{PC: pcAlpha}
		fb.regs[2] = debugger.Registers{PC: pcBeta}
	})

	AfterEach(func() {
		_ = d.Kill()
		if !fb.stopped {
			close(fb.stopCh)
			fb.stopped = true
		}
	})

	// stopOnThread2 drives a breakpoint stop on TID 2, leaving the engine
	// suspended with curTID==2 (thread 2 in beta) while threads[0]==1 is alpha.
	stopOnThread2 := func() {
		debugger.ExportedForceSuspended(d)
		debugger.ExportedSetBreakpointAt(d, pcBeta)
		Expect(d.Continue()).To(Succeed())
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 2, PC: pcBeta})
		Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventBreakpointHit))
	}

	It("Locals reads the stopped thread's frame, not threads[0]", func() {
		stopOnThread2()

		vars, err := d.Locals(0)
		Expect(err).NotTo(HaveOccurred())

		names := varNames(vars)
		Expect(names).To(ContainElement("b"),
			"Locals must return beta's local from the stopped thread (2), got %v", names)
		Expect(names).NotTo(ContainElement("a"),
			"Locals must not return alpha's local from threads[0], got %v", names)
	})

	It("StackFrames walks the stopped thread, not threads[0]", func() {
		stopOnThread2()

		frames, err := d.StackFrames()
		Expect(err).NotTo(HaveOccurred())
		Expect(frames).NotTo(BeEmpty())
		Expect(frames[0].Location.Function).To(ContainSubstring("beta"),
			"innermost frame should be beta (thread 2), got %q", frames[0].Location.Function)
	})

	It("Goroutines report the stopped thread's location", func() {
		stopOnThread2()

		gs, err := d.Goroutines()
		Expect(err).NotTo(HaveOccurred())
		Expect(gs).To(HaveLen(1))
		Expect(gs[0].CurrentLoc.Function).To(ContainSubstring("beta"),
			"goroutine location should track the stopped thread (2), got %q", gs[0].CurrentLoc.Function)
	})

	It("StackFrames tracks curTID after a step clears lastBPTID", func() {
		stopOnThread2()

		// StepInto over the breakpoint completes on thread 2. resumeFromBreakpoint
		// zeroes lastBPTID, so a StackFrames that keyed off lastBPTID would fall
		// back to threads[0] (alpha). curTID stays 2 (beta).
		Expect(d.StepInto()).To(Succeed())
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 2, PC: pcBeta})
		Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventStepped))

		frames, err := d.StackFrames()
		Expect(err).NotTo(HaveOccurred())
		Expect(frames).NotTo(BeEmpty())
		Expect(frames[0].Location.Function).To(ContainSubstring("beta"),
			"after a step, StackFrames should follow curTID (2), got %q", frames[0].Location.Function)
	})
})
