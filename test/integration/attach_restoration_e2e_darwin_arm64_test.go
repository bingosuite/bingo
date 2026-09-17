//go:build e2e && darwin && arm64 && bingonative

package integration

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

//go:embed testdata/darwin_attach_target.c
var darwinAttachTargetSource string

type darwinAttachStatus struct {
	Event          string `json:"event"`
	PID            int    `json:"pid"`
	Mask           uint32 `json:"mask"`
	Port           uint32 `json:"port"`
	Behavior       int32  `json:"behavior"`
	Flavor         int32  `json:"flavor"`
	Count          uint32 `json:"count"`
	Heartbeat      uint64 `json:"heartbeat"`
	WorkerProgress uint64 `json:"worker_progress"`
	CustomTraps    uint64 `json:"custom_traps"`
	NativeTraps    uint64 `json:"native_traps"`
	Instruction    uint32 `json:"instruction"`
}

type darwinAttachResponse struct {
	status darwinAttachStatus
	err    error
}

type darwinAttachTarget struct {
	cmd        *exec.Cmd
	input      io.WriteCloser
	responses  chan darwinAttachResponse
	exited     chan struct{}
	exitErr    error
	readerEnd  chan struct{}
	readerStop chan struct{}
	readerOnce sync.Once
	gate       string
	dwarf      string
	line       int
}

func startDarwinAttachTarget(mode string) *darwinAttachTarget {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	source := filepath.Join(dir, "darwin_attach_target.c")
	Expect(os.WriteFile(source, []byte(darwinAttachTargetSource), 0600)).To(Succeed())
	bin := filepath.Join(dir, "attach-target")
	build := exec.Command("clang", "-O0", "-g", "-gdwarf-4", "-fno-omit-frame-pointer",
		"-pthread", "-Wall", "-Wextra", "-Werror", source, "-o", bin)
	output, err := build.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "build native attach fixture: %s", output)
	dwarf := bin + ".dSYM/Contents/Resources/DWARF/attach-target"
	_, err = os.Stat(dwarf)
	Expect(err).NotTo(HaveOccurred(), "fixture DWARF companion")
	target := &darwinAttachTarget{
		gate: filepath.Join(dir, "workers-enabled"), dwarf: dwarf,
		line: markerLine(darwinAttachTargetSource, "// BINGO_BP"), responses: make(chan darwinAttachResponse, 32),
		exited: make(chan struct{}), readerEnd: make(chan struct{}), readerStop: make(chan struct{}),
	}
	target.cmd = exec.Command(bin, mode, target.gate)
	target.cmd.Stderr = GinkgoWriter
	target.input, err = target.cmd.StdinPipe()
	Expect(err).NotTo(HaveOccurred())
	stdout, err := target.cmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	Expect(target.cmd.Start()).To(Succeed())
	DeferCleanup(func() { Expect(target.close()).To(Succeed()) })
	go func() {
		defer close(target.readerEnd)
		defer close(target.responses)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var status darwinAttachStatus
			err := json.Unmarshal(scanner.Bytes(), &status)
			select {
			case target.responses <- darwinAttachResponse{status: status, err: err}:
			case <-target.readerStop:
				return
			}
			if err != nil {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case target.responses <- darwinAttachResponse{err: err}:
			case <-target.readerStop:
			}
		}
	}()
	go func() {
		<-target.readerEnd
		target.exitErr = target.cmd.Wait()
		close(target.exited)
	}()
	return target
}

func (target *darwinAttachTarget) read(event string) darwinAttachStatus {
	GinkgoHelper()
	select {
	case reply, ok := <-target.responses:
		Expect(ok).To(BeTrue(), "fixture exited while awaiting %s", event)
		Expect(reply.err).NotTo(HaveOccurred())
		Expect(reply.status.Event).To(Equal(event))
		return reply.status
	case <-time.After(3 * time.Second):
		Fail(fmt.Sprintf("native fixture PID %d did not answer %s", target.cmd.Process.Pid, event))
		return darwinAttachStatus{}
	}
}

func (target *darwinAttachTarget) command(command string) darwinAttachStatus {
	GinkgoHelper()
	_, err := fmt.Fprintln(target.input, command)
	Expect(err).NotTo(HaveOccurred())
	return target.read(command)
}

func (target *darwinAttachTarget) waitForExit() error {
	select {
	case <-target.exited:
		return target.exitErr
	case <-time.After(3 * time.Second):
		return fmt.Errorf("native fixture PID %d was not reaped", target.cmd.Process.Pid)
	}
}

func (target *darwinAttachTarget) close() error {
	defer target.input.Close()
	target.readerOnce.Do(func() { close(target.readerStop) })
	select {
	case <-target.exited:
		return target.exitErr
	default:
	}
	_, writeErr := fmt.Fprintln(target.input, "exit")
	closeErr := target.input.Close()
	exitErr := target.waitForExit()
	if exitErr != nil {
		killErr := target.cmd.Process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
		return errors.Join(writeErr, closeErr, exitErr, killErr, target.waitForExit())
	}
	return errors.Join(writeErr, closeErr)
}

func newDarwinRestorationHarness(mode string) (*darwinAttachTarget, darwinAttachStatus, *e2eHarness) {
	GinkgoHelper()
	target := startDarwinAttachTarget(mode)
	d := debugger.New(nil)
	DeferCleanup(func() {
		done := make(chan error, 1)
		go func() { done <- d.Kill() }()
		var detachErr error
		pending := false
		select {
		case detachErr = <-done:
		case <-time.After(12 * time.Second):
			pending = true
			detachErr = errors.New("cleanup could not join the native attached debugger")
		}
		if detachErr != nil {
			terminateErr := debugger.DarwinTerminateTestTarget(d, target.cmd.Process.Pid)
			killErr := target.cmd.Process.Kill()
			if errors.Is(killErr, os.ErrProcessDone) {
				killErr = nil
			}
			target.readerOnce.Do(func() { close(target.readerStop) })
			_ = target.input.Close()
			exitErr := target.waitForExit()
			retry := make(chan error, 1)
			go func() { retry <- d.Kill() }()
			var retryErr error
			select {
			case retryErr = <-retry:
			case <-time.After(12 * time.Second):
				retryErr = errors.New("failed-detach owner did not retire after test-only victim kill")
			}
			if pending {
				select {
				case err := <-done:
					detachErr = errors.Join(detachErr, err)
				case <-time.After(time.Second):
					detachErr = errors.Join(detachErr, errors.New("original cleanup call is still pending"))
				}
			}
			Fail(fmt.Sprintf("cleanup retained attached ownership: %v",
				errors.Join(detachErr, terminateErr, killErr, exitErr, retryErr)))
		}
	})
	before := target.read("ready")
	Expect(d.Attach(before.PID, target.dwarf)).To(Succeed())
	h := &e2eHarness{d: d}
	h.waitFor(3*time.Second, protocol.EventStepped)
	return target, before, h
}

type darwinDetachCall struct {
	done chan struct{}
	err  error
}

func startDarwinDetach(d debugger.Debugger, gate *debugger.DarwinWaitGate) *darwinDetachCall {
	call := &darwinDetachCall{done: make(chan struct{})}
	go func() {
		call.err = d.Kill()
		close(call.done)
	}()
	DeferCleanup(func() {
		gate.Release()
		Eventually(call.done, 12*time.Second).Should(BeClosed(), "join the spec's detach call")
	})
	return call
}

func inspectDarwinDetach(d debugger.Debugger) debugger.DarwinDetachProbe {
	GinkgoHelper()
	probe, err := debugger.DarwinInspectDetach(d)
	Expect(err).NotTo(HaveOccurred())
	return probe
}

func expectDarwinDetachComplete(d debugger.Debugger) debugger.DarwinDetachProbe {
	GinkgoHelper()
	probe := inspectDarwinDetach(d)
	Expect(probe.Latched).To(BeTrue())
	Expect(probe.WaitAcknowledged).To(BeTrue())
	Expect(probe.Retained).To(BeFalse())
	Expect(probe.Complete).To(BeTrue())
	Expect(probe.QueuedExceptions).To(BeZero())
	return probe
}

func expectDarwinHandlerRestored(before, after darwinAttachStatus) {
	GinkgoHelper()
	Expect(after.PID).To(Equal(before.PID))
	Expect(after.Count).To(Equal(before.Count))
	Expect(after.Mask).To(Equal(before.Mask))
	Expect(after.Port).To(Equal(before.Port))
	Expect(after.Behavior).To(Equal(before.Behavior))
	Expect(after.Flavor).To(Equal(before.Flavor))
	Expect(after.Instruction).To(Equal(uint32(0xd503201f)), "restored original NOP")
}

func declareDarwinAttachRestorationSpec() {
	for _, mode := range []string{"custom", "none"} {
		for _, suspended := range []bool{false, true} {
			It(fmt.Sprintf("restores the %s handler after attached detach (breakpoint-stopped=%v)", mode, suspended),
				Label("attach", "attach-restoration"), func() {
					target, before, h := newDarwinRestorationHarness(mode)
					d := h.d
					_, err := d.SetBreakpoint("darwin_attach_target.c", target.line)
					Expect(err).NotTo(HaveOccurred())
					if suspended {
						Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
					}
					Expect(d.Continue()).To(Succeed())
					if suspended {
						stop := h.waitFor(3*time.Second, protocol.EventBreakpointHit, protocol.EventError)
						Expect(stop.Kind).To(Equal(protocol.EventBreakpointHit), "%s", stop.Payload)
					} else {
						running := target.command("status")
						Expect(running.Heartbeat).To(BeNumerically(">", before.Heartbeat))
						Expect(running.Port).NotTo(Equal(before.Port))
						Expect(running.Instruction).To(Equal(uint32(0xd4200000)))
					}
					Expect(d.Kill()).To(Succeed(), "restore and detach without killing the victim")
					complete := expectDarwinDetachComplete(d)
					if suspended {
						Expect(complete.ExecuteRendezvous).To(BeNumerically(">", 0),
							"a real stopped BRK must cross a distinct-class execution boundary")
					}
					Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
					after := target.command("status")
					expectDarwinHandlerRestored(before, after)
					for i := 0; i < 4; i++ {
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
					progress := target.command("status")
					Expect(progress.Heartbeat).To(BeNumerically(">", after.Heartbeat))
					Expect(expectDarwinDetachComplete(d).Exceptions).To(Equal(complete.Exceptions),
						"no retired waiter may receive a post-detach BRK")
					AddReportEntry("restored-handler", progress)
				})
		}
	}
	for _, mode := range []string{"custom", "none"} {
		for _, boundary := range []string{"before-receive", "after-receive", "before-return"} {
			It(fmt.Sprintf("joins the %s waiter at %s and retires its real exceptions", mode, boundary),
				Label("attach", "attach-restoration", "attach-waiter"), func() {
					target, before, h := newDarwinRestorationHarness(mode)
					d := h.d
					_, err := d.SetBreakpoint("darwin_attach_target.c", target.line)
					Expect(err).NotTo(HaveOccurred())
					gate, err := debugger.DarwinGateWait(d, boundary)
					Expect(err).NotTo(HaveOccurred())
					DeferCleanup(gate.Release)
					Expect(d.Continue()).To(Succeed())
					Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
					Eventually(gate.Entered, 2*time.Second).Should(BeClosed())
					if boundary == "before-receive" {
						Eventually(func() uint32 { return inspectDarwinDetach(d).QueuedExceptions },
							2*time.Second, time.Millisecond).Should(BeNumerically(">", 0))
					} else {
						Expect(inspectDarwinDetach(d).Exceptions).To(BeNumerically(">", 0))
					}
					detached := startDarwinDetach(d, gate)
					Eventually(func() bool { return inspectDarwinDetach(d).Latched },
						time.Second, time.Millisecond).Should(BeTrue())
					pending := inspectDarwinDetach(d)
					Expect(pending.Retained).To(BeTrue())
					Expect(pending.WaitAcknowledged).To(BeFalse())
					Expect(pending.Complete).To(BeFalse())
					Expect(detached.done).NotTo(BeClosed(), "sending the wake is not a waiter join")
					gate.Release()
					Eventually(detached.done, 3*time.Second).Should(BeClosed())
					Expect(detached.err).NotTo(HaveOccurred())
					complete := expectDarwinDetachComplete(d)
					Expect(complete.ExecuteRendezvous).To(BeNumerically(">", 0),
						"queued and received BRKs must not be retired by queue emptiness alone")
					if boundary == "before-receive" {
						Expect(complete.TeardownExceptions).To(BeNumerically(">", 0),
							"teardown must actually consume the queued-unread RPC")
					}
					after := target.command("trap")
					expectDarwinHandlerRestored(before, after)
					Eventually(func() uint64 { return target.command("status").WorkerProgress },
						2*time.Second, 10*time.Millisecond).Should(BeNumerically(">", after.WorkerProgress))
					Expect(expectDarwinDetachComplete(d).Exceptions).To(Equal(complete.Exceptions))
					AddReportEntry("acknowledged-native-waiter", complete)
				})
		}
		It(fmt.Sprintf("retains the %s victim while an old exception sender withholds retirement", mode),
			Label("attach", "attach-restoration", "attach-waiter"), func() {
				target, before, h := newDarwinRestorationHarness(mode)
				d := h.d
				_, err := d.SetBreakpoint("darwin_attach_target.c", target.line)
				Expect(err).NotTo(HaveOccurred())
				release, err := debugger.DarwinHoldExceptionSender(d)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { Expect(release()).To(Succeed()) })
				Expect(d.Continue()).To(Succeed())
				err = d.Kill()
				Expect(errors.Is(err, debugger.ErrAttachedDetachIncomplete)).To(BeTrue(), "%v", err)
				Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "%v", err)
				pending := inspectDarwinDetach(d)
				Expect(pending.Latched).To(BeTrue())
				Expect(pending.WaitAcknowledged).To(BeTrue())
				Expect(pending.Retained).To(BeTrue())
				Expect(pending.Complete).To(BeFalse())
				Expect(errors.Is(d.Continue(), debugger.ErrAttachedDetachIncomplete)).To(BeTrue())
				Expect(release()).To(Succeed())
				Expect(d.Kill()).To(Succeed(), "retry must retain restoration and send-right bookkeeping")
				expectDarwinDetachComplete(d)
				Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
				after := target.command("trap")
				expectDarwinHandlerRestored(before, after)
				Eventually(func() uint64 { return target.command("status").WorkerProgress },
					2*time.Second, 10*time.Millisecond).Should(BeNumerically(">", after.WorkerProgress))
			})
		It(fmt.Sprintf("retains the %s waiter through a native join timeout and retries", mode),
			Label("attach", "attach-restoration", "attach-waiter"), func() {
				target, before, h := newDarwinRestorationHarness(mode)
				d := h.d
				_, err := d.SetBreakpoint("darwin_attach_target.c", target.line)
				Expect(err).NotTo(HaveOccurred())
				gate, err := debugger.DarwinGateWait(d, "before-return")
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(gate.Release)
				Expect(d.Continue()).To(Succeed())
				Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
				Eventually(gate.Entered, 2*time.Second).Should(BeClosed())
				err = d.Kill()
				Expect(errors.Is(err, debugger.ErrAttachedDetachIncomplete)).To(BeTrue(), "%v", err)
				Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "%v", err)
				pending := inspectDarwinDetach(d)
				Expect(pending.Latched).To(BeTrue())
				Expect(pending.WaitAcknowledged).To(BeFalse())
				Expect(pending.Retained).To(BeTrue())
				Expect(pending.Complete).To(BeFalse())
				Expect(errors.Is(d.Continue(), debugger.ErrAttachedDetachIncomplete)).To(BeTrue())
				gate.Release()
				Expect(d.Kill()).To(Succeed(), "retry must join that exact late waiter")
				expectDarwinDetachComplete(d)
				after := target.command("trap")
				expectDarwinHandlerRestored(before, after)
				Eventually(func() uint64 { return target.command("status").WorkerProgress },
					2*time.Second, 10*time.Millisecond).Should(BeNumerically(">", after.WorkerProgress))
			})
	}
	for _, mode := range []string{"custom", "none"} {
		It(fmt.Sprintf("restores the %s victim after a patched instruction failed before table insertion", mode),
			Label("attach", "attach-restoration", "attach-rendezvous"), func() {
				target, before, h := newDarwinRestorationHarness(mode)
				d := h.d
				Expect(debugger.DarwinFailNextPatchWrite(d)).To(Succeed())
				_, err := d.SetBreakpoint("darwin_attach_target.c", target.line)
				Expect(errors.Is(err, debugger.ErrAttachedDetachIncomplete)).To(BeTrue(), "%v", err)
				_, inspectErr := d.Goroutines()
				Expect(errors.Is(inspectErr, debugger.ErrAttachedDetachIncomplete)).To(BeTrue(),
					"a failed patch without an active waiter still permits only cleanup")
				instruction, err := debugger.DarwinInspectUntrackedPatch(d, before.PID)
				Expect(err).NotTo(HaveOccurred())
				Expect(instruction).To(Equal(uint32(0xd4200000)),
					"the failure must leave a real trap outside the engine breakpoint table")
				Expect(d.Kill()).To(Succeed())
				expectDarwinDetachComplete(d)
				Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
				after := target.command("trap")
				expectDarwinHandlerRestored(before, after)
				Eventually(func() uint64 { return target.command("status").WorkerProgress },
					2*time.Second, 10*time.Millisecond).Should(BeNumerically(">", after.WorkerProgress))
			})
		It(fmt.Sprintf("restores the %s victim after an asynchronous breakpoint reinstall partially fails", mode),
			Label("attach", "attach-restoration", "attach-rendezvous"), func() {
				target, before, h := newDarwinRestorationHarness(mode)
				d := h.d
				_, err := d.SetBreakpoint("darwin_attach_target.c", target.line)
				Expect(err).NotTo(HaveOccurred())
				Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
				Expect(d.Continue()).To(Succeed())
				h.waitFor(3*time.Second, protocol.EventBreakpointHit)
				gate, tid, err := debugger.DarwinGateStepStop(d, before.PID)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(gate.Release)
				Expect(d.StepInto()).To(Succeed())
				Eventually(gate.Entered, 2*time.Second).Should(BeClosed())
				Expect(debugger.DarwinVerifyStepStop(d, before.PID, tid)).To(Succeed())
				Expect(debugger.DarwinFailNextPatchWrite(d)).To(Succeed())
				gate.Release()
				failure := h.waitFor(3*time.Second, protocol.EventError)
				Expect(string(failure.Payload)).To(ContainSubstring("attached detach incomplete"))
				Expect(string(failure.Payload)).To(ContainSubstring("reinstall breakpoint"))
				h.waitFor(3*time.Second, protocol.EventPaused)
				_, inspectErr := d.Goroutines()
				Expect(errors.Is(inspectErr, debugger.ErrAttachedDetachIncomplete)).To(BeTrue(),
					"an asynchronous patch failure must also restrict admission to cleanup")
				instruction, err := debugger.DarwinInspectUntrackedPatch(d, before.PID)
				Expect(err).NotTo(HaveOccurred())
				Expect(instruction).To(Equal(uint32(0xd4200000)))
				Expect(d.Kill()).To(Succeed())
				expectDarwinDetachComplete(d)
				after := target.command("trap")
				expectDarwinHandlerRestored(before, after)
				Eventually(func() uint64 { return target.command("status").WorkerProgress },
					2*time.Second, 10*time.Millisecond).Should(BeNumerically(">", after.WorkerProgress))
			})
		It(fmt.Sprintf("retires an in-flight hardware step before restoring the %s handler", mode),
			Label("attach", "attach-restoration", "attach-rendezvous"), func() {
				target, before, h := newDarwinRestorationHarness(mode)
				d := h.d
				_, err := d.SetBreakpoint("darwin_attach_target.c", target.line)
				Expect(err).NotTo(HaveOccurred())
				Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
				Expect(d.Continue()).To(Succeed())
				h.waitFor(3*time.Second, protocol.EventBreakpointHit)
				gate, tid, err := debugger.DarwinGateStepStop(d, before.PID)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(gate.Release)
				Expect(d.StepInto()).To(Succeed())
				Eventually(gate.Entered, 2*time.Second).Should(BeClosed())
				Expect(debugger.DarwinVerifyStepStop(d, before.PID, tid)).To(Succeed())
				detached := startDarwinDetach(d, gate)
				Eventually(func() bool { return inspectDarwinDetach(d).Latched },
					time.Second, time.Millisecond).Should(BeTrue())
				gate.Release()
				Eventually(detached.done, 3*time.Second).Should(BeClosed())
				Expect(detached.err).NotTo(HaveOccurred())
				complete := expectDarwinDetachComplete(d)
				Expect(complete.ExecuteRendezvous).To(BeNumerically(">", 0))
				Expect(complete.RendezvousAcks[tid]).To(Equal(uint64(1)),
					"the old step's exact owner must acknowledge a different class")
				after := target.command("trap")
				expectDarwinHandlerRestored(before, after)
				Eventually(func() uint64 { return target.command("status").WorkerProgress },
					2*time.Second, 10*time.Millisecond).Should(BeNumerically(">", after.WorkerProgress))
				AddReportEntry("retired-in-flight-step", complete)
			})
		It(fmt.Sprintf("uses a genuine step boundary after an older hardware execution trap (%s)", mode),
			Label("attach", "attach-restoration", "attach-rendezvous"), func() {
				target, before, h := newDarwinRestorationHarness(mode)
				d := h.d
				bp, err := d.SetBreakpoint("darwin_attach_target.c", target.line)
				Expect(err).NotTo(HaveOccurred())
				Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
				Expect(d.Continue()).To(Succeed())
				h.waitFor(3*time.Second, protocol.EventBreakpointHit)
				Expect(d.ClearBreakpoint(bp.ID)).To(Succeed())
				gate, err := debugger.DarwinGateWait(d, "after-receive")
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(gate.Release)
				control, err := debugger.DarwinPrimeExecuteStop(d, before.PID)
				if control != nil {
					DeferCleanup(func() { Expect(control.Release()).To(Succeed()) })
				}
				Expect(err).NotTo(HaveOccurred())
				Eventually(gate.Entered, 2*time.Second).Should(BeClosed())
				Expect(control.VerifyExecuteStop()).To(Succeed(), "native saved ESR must actually be EC30")
				Expect(control.Release()).To(Succeed())
				detached := startDarwinDetach(d, gate)
				Eventually(func() bool { return inspectDarwinDetach(d).Latched },
					time.Second, time.Millisecond).Should(BeTrue())
				gate.Release()
				Eventually(detached.done, 3*time.Second).Should(BeClosed())
				Expect(detached.err).NotTo(HaveOccurred())
				complete := expectDarwinDetachComplete(d)
				Expect(complete.StepRendezvous).To(Equal(uint64(1)),
					"an older EC30 may be retired only by a new EC32, not another same-class trap")
				Expect(complete.RendezvousAcks[control.TID]).To(Equal(uint64(1)))
				after := target.command("trap")
				expectDarwinHandlerRestored(before, after)
				Eventually(func() uint64 { return target.command("status").WorkerProgress },
					2*time.Second, 10*time.Millisecond).Should(BeNumerically(">", after.WorkerProgress))
				AddReportEntry("distinct-step-rendezvous", complete)
			})
		It(fmt.Sprintf("retains an externally held %s thread until its rendezvous can run", mode),
			Label("attach", "attach-restoration", "attach-rendezvous"), func() {
				target, before, h := newDarwinRestorationHarness(mode)
				d := h.d
				_, err := d.SetBreakpoint("darwin_attach_target.c", target.line)
				Expect(err).NotTo(HaveOccurred())
				Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
				Expect(d.Continue()).To(Succeed())
				h.waitFor(3*time.Second, protocol.EventBreakpointHit)
				control, err := debugger.DarwinHoldStoppedThread(d, before.PID)
				if control != nil {
					DeferCleanup(func() { Expect(control.Release()).To(Succeed()) })
				}
				Expect(err).NotTo(HaveOccurred())
				err = d.Kill()
				Expect(errors.Is(err, debugger.ErrAttachedDetachIncomplete)).To(BeTrue(), "%v", err)
				Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "%v", err)
				pending := inspectDarwinDetach(d)
				Expect(pending.WaitAcknowledged).To(BeTrue())
				Expect(pending.Retained).To(BeTrue())
				Expect(pending.Complete).To(BeFalse())
				Expect(pending.RendezvousArms[control.TID]).To(BeNumerically(">", 0))
				Expect(pending.RendezvousAcks[control.TID]).To(BeZero())
				Expect(errors.Is(d.Continue(), debugger.ErrAttachedDetachIncomplete)).To(BeTrue())
				Expect(control.Release()).To(Succeed())
				Expect(d.Kill()).To(Succeed())
				complete := expectDarwinDetachComplete(d)
				Expect(complete.RendezvousAcks[control.TID]).To(Equal(uint64(1)))
				after := target.command("trap")
				expectDarwinHandlerRestored(before, after)
				Eventually(func() uint64 { return target.command("status").WorkerProgress },
					2*time.Second, 10*time.Millisecond).Should(BeNumerically(">", after.WorkerProgress))
			})
	}
	for _, boundary := range []string{"before-rendezvous-ack", "after-rendezvous-ack"} {
		It("retains a received rendezvous across "+boundary+" without executing again",
			Label("attach", "attach-restoration", "attach-rendezvous"), func() {
				target, before, h := newDarwinRestorationHarness("custom")
				d := h.d
				_, err := d.SetBreakpoint("darwin_attach_target.c", target.line)
				Expect(err).NotTo(HaveOccurred())
				Expect(os.WriteFile(target.gate, nil, 0600)).To(Succeed())
				Expect(d.Continue()).To(Succeed())
				h.waitFor(3*time.Second, protocol.EventBreakpointHit)
				gate, err := debugger.DarwinGateWait(d, boundary)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(gate.Release)
				err = d.Kill()
				Expect(gate.Entered).To(BeClosed())
				Expect(errors.Is(err, debugger.ErrAttachedDetachIncomplete)).To(BeTrue(), "%v", err)
				Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "%v", err)
				pending := inspectDarwinDetach(d)
				Expect(pending.Retained).To(BeTrue())
				Expect(pending.Complete).To(BeFalse())
				Expect(pending.RendezvousArms).To(HaveLen(1),
					"the first genuine marker must be retained rather than running later threads")
				var tid int
				for thread := range pending.RendezvousArms {
					tid = thread
				}
				gate.Release()
				Expect(d.Kill()).To(Succeed())
				complete := expectDarwinDetachComplete(d)
				Expect(complete.RendezvousArms[tid]).To(Equal(pending.RendezvousArms[tid]),
					"retry must finish the received marker, not re-arm or execute its thread")
				Expect(complete.RendezvousAcks[tid]).To(Equal(uint64(1)))
				after := target.command("trap")
				expectDarwinHandlerRestored(before, after)
				Eventually(func() uint64 { return target.command("status").WorkerProgress },
					2*time.Second, 10*time.Millisecond).Should(BeNumerically(">", after.WorkerProgress))
			})
	}
}
