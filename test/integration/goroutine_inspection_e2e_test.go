//go:build e2e && ((linux && amd64) || (darwin && arm64 && bingonative))

package integration

import (
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"
	godap "github.com/google/go-dap"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const goroutineInspectionTarget = `package main
import (
	"fmt"
	"runtime"
	"sync/atomic"
	"time"
)
var ready = make(chan struct{}, 3)
var parked = make(chan struct{})
var counter int64
func waitingWorker(tag int) {
	label := tag
	ready <- struct{}{}
	<-parked
	fmt.Println(label)
}
func runningWorker(tag int) {
	runtime.LockOSThread()
	label := tag
	ready <- struct{}{}
	for {
		atomic.AddInt64(&counter, int64(label))
	}
}
func main() {
	go waitingWorker(101)
	go waitingWorker(202)
	go runningWorker(303)
	for i := 0; i < 3; i++ { <-ready }
	for {
		fmt.Println(counter) // INSPECT_STOP
		time.Sleep(10*time.Millisecond)
	}
}
`

func declareGoroutineInspectionSpec() {
	It("inspects distinct parked and running goroutines across stable selected requests", Label("inspect"), func() {
		bin := buildTarget("goroutine_inspection", goroutineInspectionTarget)
		h := newE2EHarness(bin)
		h.waitFor(15*time.Second, protocol.EventStepped)
		_, err := h.d.SetBreakpoint("goroutine_inspection.go", markerLine(goroutineInspectionTarget, "// INSPECT_STOP"))
		Expect(err).NotTo(HaveOccurred())
		Expect(h.d.Continue()).To(Succeed())
		h.waitFor(20*time.Second, protocol.EventBreakpointHit)
		list, err := h.d.Goroutines()
		Expect(err).NotTo(HaveOccurred())
		waiting := make(map[string]bool)
		running := false
		for _, g := range list.Goroutines {
			if !isInspectionFixtureWorker(g.StartLoc.Function) {
				continue
			}
			label, isRunning := inspectNativeFixtureWorker(h.d, g.ID)
			if isRunning {
				Expect(label).To(Equal("303"))
				running = true
			} else {
				waiting[label] = true
			}
		}
		Expect(waiting).To(Equal(map[string]bool{"101": true, "202": true}))
		Expect(running).To(BeTrue())
		_, err = debugger.StackFramesForGoroutine(h.d, -1)
		Expect(err).To(HaveOccurred())
		_, err = debugger.StackFramesForGoroutine(h.d, 1<<53)
		Expect(err).To(HaveOccurred())
		Expect(h.d.Continue()).To(Succeed())
		h.waitFor(20*time.Second, protocol.EventBreakpointHit)
		Expect(h.d.Kill()).To(Succeed(), "Kill must retire the retained inspection hold")
	})
}

func isInspectionFixtureWorker(function string) bool {
	switch function {
	case "main.main.gowrap1", "main.main.gowrap2", "main.main.gowrap3":
		return true
	default:
		return false
	}
}

func inspectNativeFixtureWorker(d debugger.Debugger, goid int) (string, bool) {
	frames, err := debugger.StackFramesForGoroutine(d, goid)
	Expect(err).NotTo(HaveOccurred(), "selected stack g%d: %v", goid, err)
	var worker protocol.Frame
	found := false
	for _, frame := range frames {
		if frame.Location.Function == "main.waitingWorker" || frame.Location.Function == "main.runningWorker" {
			worker, found = frame, true
			break
		}
	}
	Expect(found).To(BeTrue(), "g%d must have its own worker frame", goid)
	vars, err := debugger.LocalsForGoroutine(d, goid, worker.Index)
	Expect(err).NotTo(HaveOccurred())
	value, err := debugger.EvaluateForGoroutine(d, goid, worker.Index, "label")
	Expect(err).NotTo(HaveOccurred())
	Expect(vars).To(ContainElement(And(HaveField("Name", "label"), HaveField("Value", value.Value))))
	again, err := debugger.StackFramesForGoroutine(d, goid)
	Expect(err).NotTo(HaveOccurred())
	Expect(again).To(Equal(frames), "selected context must remain stable across requests")
	return value.Value, worker.Location.Function == "main.runningWorker"
}

func declareDAPGoroutineInspectionSpec() {
	It("keeps selected DAP stacks, scopes, locals and evaluate distinct", Label("dap"), func() {
		bin := buildTarget("dap_goroutine_inspection", goroutineInspectionTarget)
		_, wsAddr, addr := startTestServerWithDAP()
		dc := dialDAP(addr)
		dc.initialize()
		start := dc.launch(bin, false)
		dc.waitEvent(20*time.Second, "initialized")
		dc.setBreakpoints("dap_goroutine_inspection.go", markerLine(goroutineInspectionTarget, "// INSPECT_STOP"))
		dc.configurationDone()
		dc.await(start)
		dc.waitStopped(20 * time.Second)
		obs, err := client.Join(wsAddr, waitForSession(wsAddr))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(obs.Close()).To(Succeed()) })
		goroutines, err := obs.Goroutines()
		Expect(err).NotTo(HaveOccurred())
		expected := make(map[int64]string)
		for _, g := range goroutines {
			switch g.StartLoc.Function {
			case "main.main.gowrap1":
				expected[int64(g.ID)] = "101"
			case "main.main.gowrap2":
				expected[int64(g.ID)] = "202"
			case "main.main.gowrap3":
				expected[int64(g.ID)] = "303"
			}
		}
		Expect(expected).To(HaveLen(3), "the graphical goid identities must name all three fixture workers")
		threads := dc.threads()
		dc.mu.Lock()
		mapping := dc.threadMetadata[threads.RequestSeq]
		delete(dc.threadMetadata, threads.RequestSeq)
		dc.mu.Unlock()
		Expect(mapping).To(HaveLen(len(threads.Body.Threads)), "every real graph goid requires explicit identity proof")
		values := make(map[int64]string)
		handles := make(map[int]bool)
		for _, thread := range threads.Body.Threads {
			goid, known := mapping[thread.Id]
			Expect(known).To(BeTrue())
			Expect(goid).To(BeNumerically(">", 0))
			label, selected := expected[goid]
			if !selected {
				continue
			}
			inspectDAPFixtureWorker(dc, thread.Id, goid, label, handles, values)
		}
		Expect(values).To(Equal(expected))
	})
}

func inspectDAPFixtureWorker(dc *dapClient, threadID int, goid int64, label string, handles map[int]bool, values map[int64]string) {
	response := dc.request("stackTrace", &godap.StackTraceRequest{
		Arguments: godap.StackTraceArguments{ThreadId: threadID},
	})
	stack, ok := response.(*godap.StackTraceResponse)
	Expect(ok).To(BeTrue(), "unexpected selected stack response %T", response)
	for _, frame := range stack.Body.StackFrames {
		if frame.Name != "main.waitingWorker" && frame.Name != "main.runningWorker" {
			continue
		}
		Expect(handles[frame.Id]).To(BeFalse(), "frames of distinct goroutines must not alias")
		handles[frame.Id] = true
		scope := dc.scopes(frame.Id)
		Expect(scope.Body.Scopes).To(HaveLen(1))
		vars := dc.variables(scope.Body.Scopes[0].VariablesReference)
		Expect(vars.GetResponse().Success).To(BeTrue())
		value := dc.evaluate("label", frame.Id, "watch")
		Expect(vars.(*godap.VariablesResponse).Body.Variables).To(ContainElement(And(HaveField("Name", "label"), HaveField("Value", value.Body.Result))))
		Expect(value.Body.Result).To(Equal(label), "graph goid %d resolved to handle %d", goid, threadID)
		values[goid] = value.Body.Result
	}
}
