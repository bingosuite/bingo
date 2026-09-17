//go:build e2e && darwin && arm64 && bingonative

package integration

import (
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

func inspectDarwinNamespace(d debugger.Debugger) debugger.DarwinNamespaceProbe {
	GinkgoHelper()
	probe, err := debugger.DarwinInspectNamespace(d)
	Expect(err).NotTo(HaveOccurred())
	return probe
}

func namespaceCensus() debugger.DarwinNamespaceCensus {
	GinkgoHelper()
	census, err := debugger.DarwinMachCensus()
	Expect(err).NotTo(HaveOccurred())
	return census
}

func expectNamespaceBaseline(baseline debugger.DarwinNamespaceCensus) debugger.DarwinNamespaceCensus {
	GinkgoHelper()
	var census debugger.DarwinNamespaceCensus
	Eventually(func(g Gomega) {
		var err error
		census, err = debugger.DarwinMachCensus()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(census.PortSets).To(Equal(baseline.PortSets), "port-set census: %+v", census)
		g.Expect(census.DeadNames).To(Equal(baseline.DeadNames), "dead-name census: %+v", census)
	}, 2*time.Second, 10*time.Millisecond).Should(Succeed())
	return census
}

func expectNamespaceNamesGone(names []uint32) {
	GinkgoHelper()
	remaining, err := debugger.DarwinRemainingMachNames(names)
	Expect(err).NotTo(HaveOccurred())
	Expect(remaining).To(BeEmpty(), "former Bingo-owned Mach names are still present")
}

func expectNamespaceClosed(d debugger.Debugger, names []uint32) []protocol.Event {
	GinkgoHelper()
	var events []protocol.Event
	deadline := time.After(12 * time.Second)
	for {
		select {
		case evt, ok := <-d.Events():
			if !ok {
				probe := inspectDarwinNamespace(d)
				Expect(probe.Released).To(BeTrue(), "event-stream close requires namespace retirement")
				Expect(probe.Eligible).To(BeTrue(), "retirement must preserve COMPLETE")
				Expect(probe.Names).To(BeEmpty(), "no ownership fields may survive successful release")
				expectNamespaceNamesGone(names)
				Expect(debugger.DarwinTryNamespaceRelease(d)).To(Succeed(), "idempotent release")
				Expect(d.Kill()).To(Succeed(), "idempotent completed Kill")
				return events
			}
			Expect(evt.Kind).NotTo(Equal(protocol.EventError), "retirement error: %s", evt.Payload)
			events = append(events, evt)
		case <-deadline:
			Fail("native engine retained its event stream after namespace retirement")
		}
	}
}

func namespaceDebugger() debugger.Debugger {
	GinkgoHelper()
	d := debugger.New(nil)
	DeferCleanup(func() {
		done := make(chan error, 1)
		go func() { done <- d.Kill() }()
		Eventually(done, 12*time.Second).Should(Receive(Succeed()), "join native namespace cleanup")
	})
	return d
}

func launchNamespaceTarget(bin string) *e2eHarness {
	GinkgoHelper()
	d := namespaceDebugger()
	Expect(d.Launch(bin, nil, nil)).To(Succeed())
	h := &e2eHarness{d: d}
	h.waitFor(15*time.Second, protocol.EventStepped)
	return h
}

func expectNamespaceVictimProgress(target *darwinAttachTarget, before darwinAttachStatus, mode string) {
	GinkgoHelper()
	Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
	after := target.command("status")
	expectDarwinHandlerRestored(before, after)
	for i := 0; i < 2; i++ {
		trap := target.command("trap")
		expectDarwinHandlerRestored(before, trap)
		if mode == "custom" {
			Expect(trap.CustomTraps).To(Equal(before.CustomTraps + uint64(i+1)))
			Expect(trap.NativeTraps).To(Equal(before.NativeTraps))
		} else {
			Expect(trap.NativeTraps).To(Equal(before.NativeTraps + uint64(i+1)))
			Expect(trap.CustomTraps).To(Equal(before.CustomTraps))
		}
	}
	Eventually(func() uint64 { return target.command("status").WorkerProgress },
		2*time.Second, 10*time.Millisecond).Should(BeNumerically(">", after.WorkerProgress))
	Expect(target.command("status").Heartbeat).To(BeNumerically(">", after.Heartbeat))
}

func declareDarwinNamespaceSpec() {
	It("returns port sets and dead names to baseline through 20 launches and 10 live detaches",
		Label("namespace"), func() {
			exitBin := buildTarget("namespace_exit", exitCodeTargetSrc)
			killBin := buildTarget("namespace_kill", basicTargetSrc)
			warm := launchNamespaceTarget(exitBin)
			Expect(warm.d.Kill()).To(Succeed())
			expectNamespaceClosed(warm.d, nil)
			baseline := namespaceCensus()

			leak, err := debugger.DarwinAllocateNamespaceLeak()
			if leak != nil {
				DeferCleanup(func() { Expect(leak.Close()).To(Succeed()) })
			}
			Expect(err).NotTo(HaveOccurred())
			leakedNames := []uint32{leak.PortSet, leak.DeadName}
			detected := namespaceCensus()
			Expect(detected.PortSets).To(Equal(baseline.PortSets + 1))
			Expect(detected.DeadNames).To(Equal(baseline.DeadNames + 1))
			Expect(leak.Close()).To(Succeed())
			Expect(leak.Close()).To(Succeed())
			expectNamespaceNamesGone(leakedNames)
			expectNamespaceBaseline(baseline)
			AddReportEntry("namespace-census-self-check", map[string]any{
				"debuggerPID": os.Getpid(), "baseline": baseline, "deliberateLeak": detected,
			})

			for i := 0; i < 20; i++ {
				natural := i%2 == 0
				bin := killBin
				if natural {
					bin = exitBin
				}
				h := launchNamespaceTarget(bin)
				if !natural {
					_, err := h.d.SetBreakpoint("namespace_kill.go", markerLine(basicTargetSrc, "// BP"))
					Expect(err).NotTo(HaveOccurred())
					Expect(h.d.Continue()).To(Succeed())
					h.waitFor(15*time.Second, protocol.EventBreakpointHit)
				}
				owned := inspectDarwinNamespace(h.d)
				Expect(owned.PortSet).NotTo(BeZero())
				Expect(owned.Task).NotTo(BeZero())
				Expect(owned.Threads).NotTo(BeEmpty())
				Expect(debugger.DarwinTryNamespaceRelease(h.d)).NotTo(Succeed(),
					"an active launch cannot authorize namespace destruction")
				if natural || i%4 == 3 {
					Expect(h.d.Continue()).To(Succeed())
				}
				if !natural {
					Expect(h.d.Kill()).To(Succeed())
				}
				events := expectNamespaceClosed(h.d, owned.Names)
				exits := 0
				for _, evt := range events {
					if evt.Kind == protocol.EventProcessExited {
						exits++
						var payload protocol.ProcessExitedPayload
						Expect(protocol.DecodeEventPayload(evt, &payload)).To(Succeed())
						Expect(payload.ExitCode).To(Equal(exitCodeTargetExpected))
					}
				}
				if natural {
					Expect(exits).To(Equal(1), "namespace release must preserve natural exit status")
				}
				after := expectNamespaceBaseline(baseline)
				AddReportEntry(fmt.Sprintf("namespace-launch-%02d", i+1), map[string]any{
					"naturalExit": natural, "ownedNames": len(owned.Names), "after": after,
				})
			}

			for i := 0; i < 10; i++ {
				mode := "custom"
				if i%2 != 0 {
					mode = "none"
				}
				stopped := i%4 < 2
				target, before, h := newDarwinRestorationHarness(mode)
				_, err := h.d.SetBreakpoint("darwin_attach_target.c", target.line)
				Expect(err).NotTo(HaveOccurred())
				if stopped {
					Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
				}
				Expect(h.d.Continue()).To(Succeed())
				if stopped {
					h.waitFor(3*time.Second, protocol.EventBreakpointHit)
				} else {
					Expect(target.command("status").Heartbeat).To(BeNumerically(">", before.Heartbeat))
				}
				owned := inspectDarwinNamespace(h.d)
				Expect(debugger.DarwinTryNamespaceRelease(h.d)).NotTo(Succeed(),
					"outstanding attached ownership must block namespace destruction")
				Expect(inspectDarwinNamespace(h.d).Names).To(Equal(owned.Names))
				Expect(h.d.Kill()).To(Succeed())
				complete := expectDarwinDetachComplete(h.d)
				expectNamespaceClosed(h.d, owned.Names)
				expectNamespaceVictimProgress(target, before, mode)
				Expect(expectDarwinDetachComplete(h.d).Exceptions).To(Equal(complete.Exceptions))
				Expect(target.close()).To(Succeed())
				after := expectNamespaceBaseline(baseline)
				AddReportEntry(fmt.Sprintf("namespace-attach-%02d", i+1), map[string]any{
					"handler": mode, "breakpointStopped": stopped, "ownedNames": len(owned.Names), "after": after,
				})
			}
			AddReportEntry("namespace-cycle-summary", map[string]any{
				"debuggerPID": os.Getpid(), "launchKill": 10, "launchNaturalExit": 10,
				"attachCustom": 5, "attachNull": 5, "baseline": baseline, "final": namespaceCensus(),
			})
		})

	for _, attached := range []bool{false, true} {
		for _, boundary := range []int{1, 6, 10} {
			It(fmt.Sprintf("unwinds real partial Mach setup at call %d (attached=%v)", boundary, attached),
				Label("namespace"), func() {
					var target *darwinAttachTarget
					var before darwinAttachStatus
					bin := ""
					if attached {
						target = startDarwinAttachTarget("custom")
						before = target.read("ready")
					} else {
						bin = buildTarget("namespace_partial", basicTargetSrc)
					}
					baseline := namespaceCensus()
					d := namespaceDebugger()
					clear, err := debugger.DarwinFaultNamespace(d, boundary, 0)
					Expect(err).NotTo(HaveOccurred())
					DeferCleanup(clear)
					if attached {
						err = d.Attach(before.PID, target.dwarf)
					} else {
						err = d.Launch(bin, nil, nil)
					}
					Expect(err).To(MatchError(ContainSubstring("native test namespace setup failure")))
					Expect(err).NotTo(MatchError(ContainSubstring(debugger.ErrBackendCleanupIncomplete.Error())))
					Expect(inspectDarwinNamespace(d).Released).To(BeTrue(), "startup must unwind before returning")
					Expect(d.Kill()).To(Succeed())
					expectNamespaceClosed(d, nil)
					if attached {
						expectNamespaceVictimProgress(target, before, "custom")
						Expect(target.close()).To(Succeed())
					}
					expectNamespaceBaseline(baseline)
				})
		}
	}

	It("retains partial setup names until failed unwind can be retried", Label("namespace"), func() {
		bin := buildTarget("namespace_retry", basicTargetSrc)
		baseline := namespaceCensus()
		d := namespaceDebugger()
		clear, err := debugger.DarwinFaultNamespace(d, 10, 3)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(clear)
		Expect(d.Launch(bin, nil, nil)).To(MatchError(ContainSubstring(debugger.ErrBackendCleanupIncomplete.Error())))
		owned := inspectDarwinNamespace(d)
		Expect(owned.Released).To(BeFalse())
		Expect(owned.Names).NotTo(BeEmpty())
		Expect(d.Kill()).To(MatchError(ContainSubstring(debugger.ErrBackendCleanupIncomplete.Error())))
		Expect(d.Launch(bin, nil, nil)).To(MatchError(ContainSubstring(debugger.ErrBackendCleanupIncomplete.Error())))
		clear()
		Expect(d.Kill()).To(Succeed())
		expectNamespaceClosed(d, owned.Names)
		expectNamespaceBaseline(baseline)
	})

	It("retries a post-COMPLETE release without touching the detached victim", Label("namespace"), func() {
		baseline := namespaceCensus()
		target, before, h := newDarwinRestorationHarness("custom")
		_, err := h.d.SetBreakpoint("darwin_attach_target.c", target.line)
		Expect(err).NotTo(HaveOccurred())
		Expect(h.d.Continue()).To(Succeed())
		owned := inspectDarwinNamespace(h.d)
		clear, err := debugger.DarwinFaultNamespace(h.d, 0, 3)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(clear)
		Expect(h.d.Kill()).To(MatchError(ContainSubstring(debugger.ErrBackendCleanupIncomplete.Error())))
		expectDarwinDetachComplete(h.d)
		partial := inspectDarwinNamespace(h.d)
		Expect(partial.Eligible).To(BeTrue())
		Expect(partial.Released).To(BeFalse())
		Expect(partial.ExceptionPort).To(BeZero(), "at least one real receiver was already released")
		expectNamespaceVictimProgress(target, before, "custom")
		Expect(h.d.Continue()).To(MatchError(ContainSubstring(debugger.ErrBackendCleanupIncomplete.Error())))
		clear()
		Expect(h.d.Kill()).To(Succeed())
		expectNamespaceClosed(h.d, owned.Names)
		Expect(target.close()).To(Succeed())
		expectNamespaceBaseline(baseline)
	})

	It("keeps every receiver while old attached RPC senders withhold COMPLETE", Label("namespace"), func() {
		baseline := namespaceCensus()
		target, before, h := newDarwinRestorationHarness("custom")
		Expect(h.d.Continue()).To(Succeed())
		releaseSender, err := debugger.DarwinHoldExceptionSender(h.d)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(releaseSender()).To(Succeed()) })
		owned := inspectDarwinNamespace(h.d)
		Expect(h.d.Kill()).To(MatchError(ContainSubstring(debugger.ErrAttachedDetachIncomplete.Error())))
		Expect(inspectDarwinDetach(h.d).Retained).To(BeTrue())
		Expect(debugger.DarwinTryNamespaceRelease(h.d)).NotTo(Succeed())
		Expect(inspectDarwinNamespace(h.d).Names).To(Equal(owned.Names))
		remaining, err := debugger.DarwinRemainingMachNames(owned.Names)
		Expect(err).NotTo(HaveOccurred())
		Expect(remaining).To(Equal(owned.Names), "incomplete restoration must preserve all actual names")
		Expect(releaseSender()).To(Succeed())
		Expect(h.d.Kill()).To(Succeed())
		expectNamespaceClosed(h.d, owned.Names)
		expectNamespaceVictimProgress(target, before, "custom")
		Expect(target.close()).To(Succeed())
		expectNamespaceBaseline(baseline)
	})

	It("releases only Bingo's coalesced task and thread references", Label("namespace"), func() {
		bin := buildTarget("namespace_coalesced", basicTargetSrc)
		baseline := namespaceCensus()
		h := launchNamespaceTarget(bin)
		owned := inspectDarwinNamespace(h.d)
		external, release, err := debugger.DarwinHoldNamespaceReferences(h.d)
		if release != nil {
			DeferCleanup(func() { Expect(release()).To(Succeed()) })
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(external).To(HaveLen(len(owned.Threads) + 1))
		Expect(h.d.Kill()).To(Succeed())
		expectNamespaceClosed(h.d, nil)
		remaining, err := debugger.DarwinRemainingMachNames(owned.Names)
		Expect(err).NotTo(HaveOccurred())
		Expect(remaining).To(ConsistOf(external), "external urefs must survive backend ownership retirement")
		Expect(namespaceCensus().DeadNames).To(Equal(baseline.DeadNames + len(external)))
		Expect(release()).To(Succeed())
		expectNamespaceNamesGone(owned.Names)
		expectNamespaceBaseline(baseline)
	})
}
