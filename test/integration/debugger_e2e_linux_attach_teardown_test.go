//go:build e2e && linux && amd64

package integration

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

const attachTeardownTargetSrc = `package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sync/atomic"
	"unsafe"
)

var heartbeat atomic.Uint64

//go:noinline
func futureBreakpoint() {
	heartbeat.Add(1000) // BP_FUTURE
}

func functionBytes() []byte {
	entry := reflect.ValueOf(futureBreakpoint).Pointer()
	return bytes.Clone(unsafe.Slice((*byte)(unsafe.Pointer(entry)), 128))
}

func main() {
	runtime.LockOSThread()
	original := functionBytes()
	go func() {
		for {
			heartbeat.Add(1)
			runtime.Gosched()
		}
	}()

	fmt.Println("READY")
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		switch input.Text() {
		case "heartbeat":
			fmt.Printf("HEARTBEAT %d\n", heartbeat.Load())
		case "check":
			current := functionBytes()
			if bytes.Equal(current, original) {
				fmt.Println("BYTES_OK")
				continue
			}
			for i := range original {
				if original[i] != current[i] {
					fmt.Printf("BYTES_BAD %d %02x %02x\n", i, original[i], current[i])
					break
				}
			}
		case "run":
			futureBreakpoint()
			fmt.Println("RAN")
		case "exit":
			fmt.Println("DONE")
			return
		}
	}
}
`

type attachTeardownProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	lines    chan string
	wait     chan error
	stderr   bytes.Buffer
	waitMu   sync.Mutex
	waitOnce sync.Once
	waitErr  error
	waited   bool
}

func startAttachTeardownProcess(bin string) *attachTeardownProcess {
	GinkgoHelper()
	p := &attachTeardownProcess{
		cmd:   exec.Command(bin),
		lines: make(chan string, 16),
		wait:  make(chan error, 1),
	}
	stdout, err := p.cmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred(), "create target stdout")
	p.stdin, err = p.cmd.StdinPipe()
	Expect(err).NotTo(HaveOccurred(), "create target stdin")
	p.cmd.Stderr = &p.stderr
	Expect(p.cmd.Start()).To(Succeed(), "start attach teardown target")

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			p.lines <- scanner.Text()
		}
		close(p.lines)
	}()
	// Cmd.Wait uses wait4 in this process. Starting it before bingo detaches
	// would compete with linuxWaitOwner and can steal a ptrace stop.
	return p
}

func (p *attachTeardownProcess) beginWait() {
	p.waitOnce.Do(func() {
		go func() {
			err := p.cmd.Wait()
			p.waitMu.Lock()
			p.waitErr = err
			p.waited = true
			p.waitMu.Unlock()
			p.wait <- err
			close(p.wait)
		}()
	})
}

func (p *attachTeardownProcess) send(command string) {
	GinkgoHelper()
	_, err := fmt.Fprintln(p.stdin, command)
	Expect(err).NotTo(HaveOccurred(), "send target command %q", command)
}

func (p *attachTeardownProcess) line(timeout time.Duration) string {
	GinkgoHelper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			p.waitMu.Lock()
			waitErr := p.waitErr
			p.waitMu.Unlock()
			Fail(fmt.Sprintf("target output closed (wait=%v, stderr=%q)", waitErr, p.stderr.String()))
		}
		return line
	case <-time.After(timeout):
		Fail(fmt.Sprintf("timeout waiting for target output (stderr=%q)", p.stderr.String()))
		return ""
	}
}

func (p *attachTeardownProcess) heartbeat(timeout time.Duration) uint64 {
	GinkgoHelper()
	p.send("heartbeat")
	line := p.line(timeout)
	fields := strings.Fields(line)
	Expect(fields).To(HaveLen(2), "heartbeat output: %q", line)
	Expect(fields[0]).To(Equal("HEARTBEAT"), "heartbeat output: %q", line)
	value, err := strconv.ParseUint(fields[1], 10, 64)
	Expect(err).NotTo(HaveOccurred(), "parse heartbeat output %q", line)
	return value
}

func (p *attachTeardownProcess) cleanup() {
	p.waitMu.Lock()
	waited := p.waited
	p.waitMu.Unlock()
	if waited {
		return
	}
	_ = p.cmd.Process.Kill()
	p.beginWait()
	select {
	case <-p.wait:
	case <-time.After(5 * time.Second):
	}
}

func tracerPID(pid int) int {
	GinkgoHelper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	Expect(err).NotTo(HaveOccurred(), "read target status")
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "TracerPid:") {
			continue
		}
		value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "TracerPid:")))
		Expect(err).NotTo(HaveOccurred(), "parse %q", line)
		return value
	}
	Fail("TracerPid missing from target status")
	return -1
}

func declareAttachTeardownSpec() {
	It("restores a running attached target before releasing it", Label("attach", "attach-teardown"), func() {
		line := markerLine(attachTeardownTargetSrc, "// BP_FUTURE")
		bin := buildTarget("attach_teardown_target", attachTeardownTargetSrc)
		target := startAttachTeardownProcess(bin)
		DeferCleanup(target.cleanup)
		Expect(target.line(10 * time.Second)).To(Equal("READY"))

		d := debugger.New(nil)
		DeferCleanup(func() { _ = d.Kill() })
		Expect(d.Attach(target.cmd.Process.Pid, bin)).To(Succeed(), "attach to target")
		awaitEvent(d.Events(), 10*time.Second, protocol.EventStepped)
		_, err := d.SetBreakpoint("attach_teardown_target.go", line)
		Expect(err).NotTo(HaveOccurred(), "arm future breakpoint")
		Expect(d.Continue()).To(Succeed(), "resume gated target")

		before := target.heartbeat(10 * time.Second)
		time.Sleep(25 * time.Millisecond)
		after := target.heartbeat(10 * time.Second)
		Expect(after).To(BeNumerically(">", before), "target heartbeat before detach")

		Expect(d.Kill()).To(Succeed(), "detach running target")
		target.beginWait()
		Eventually(func() int { return tracerPID(target.cmd.Process.Pid) }, 5*time.Second, 10*time.Millisecond).
			Should(Equal(0), "target must no longer be traced")

		target.send("check")
		Expect(target.line(5*time.Second)).To(Equal("BYTES_OK"),
			"the target text must match its pre-attach bytes")
		detachedBefore := target.heartbeat(5 * time.Second)
		time.Sleep(25 * time.Millisecond)
		detachedAfter := target.heartbeat(5 * time.Second)
		Expect(detachedAfter).To(BeNumerically(">", detachedBefore), "target heartbeat after detach")

		target.send("run")
		Expect(target.line(5*time.Second)).To(Equal("RAN"),
			"the former breakpoint must execute normally after detach")
		target.send("exit")
		Expect(target.line(5 * time.Second)).To(Equal("DONE"))
		select {
		case err := <-target.wait:
			Expect(err).NotTo(HaveOccurred(),
				"target must exit cleanly after executing the restored instruction; stderr=%q", target.stderr.String())
		case <-time.After(5 * time.Second):
			Fail("target did not exit after executing the former breakpoint")
		}
	})

	It("rewinds and restores an attached target already stopped at the breakpoint", Label("attach", "attach-teardown"), func() {
		line := markerLine(attachTeardownTargetSrc, "// BP_FUTURE")
		bin := buildTarget("attach_teardown_suspended_target", attachTeardownTargetSrc)
		target := startAttachTeardownProcess(bin)
		DeferCleanup(target.cleanup)
		Expect(target.line(10 * time.Second)).To(Equal("READY"))

		d := debugger.New(nil)
		DeferCleanup(func() { _ = d.Kill() })
		Expect(d.Attach(target.cmd.Process.Pid, bin)).To(Succeed(), "attach to target")
		awaitEvent(d.Events(), 10*time.Second, protocol.EventStepped)
		_, err := d.SetBreakpoint("attach_teardown_suspended_target.go", line)
		Expect(err).NotTo(HaveOccurred(), "arm breakpoint")
		Expect(d.Continue()).To(Succeed())
		target.send("run")
		event := awaitEvent(d.Events(), 10*time.Second,
			protocol.EventBreakpointHit, protocol.EventError, protocol.EventProcessExited)
		Expect(event.Kind).To(Equal(protocol.EventBreakpointHit), "target reached breakpoint")

		Expect(d.Kill()).To(Succeed(), "detach suspended target")
		target.beginWait()
		Expect(target.line(5*time.Second)).To(Equal("RAN"),
			"detach must resume at the restored instruction, not mid-instruction")
		target.send("check")
		Expect(target.line(5 * time.Second)).To(Equal("BYTES_OK"))
		target.send("exit")
		Expect(target.line(5 * time.Second)).To(Equal("DONE"))
		select {
		case err := <-target.wait:
			Expect(err).NotTo(HaveOccurred(), "suspended target exit; stderr=%q", target.stderr.String())
		case <-time.After(5 * time.Second):
			Fail("suspended target did not exit after detach")
		}
	})

	It("detaches a running attached target without breakpoints as a control", Label("attach", "attach-teardown"), func() {
		bin := buildTarget("attach_teardown_control_target", attachTeardownTargetSrc)
		target := startAttachTeardownProcess(bin)
		DeferCleanup(target.cleanup)
		Expect(target.line(10 * time.Second)).To(Equal("READY"))

		d := debugger.New(nil)
		DeferCleanup(func() { _ = d.Kill() })
		Expect(d.Attach(target.cmd.Process.Pid, bin)).To(Succeed(), "attach to target")
		awaitEvent(d.Events(), 10*time.Second, protocol.EventStepped)
		Expect(d.Continue()).To(Succeed())
		Expect(d.Kill()).To(Succeed(), "detach running control")
		target.beginWait()
		Eventually(func() int { return tracerPID(target.cmd.Process.Pid) }, 5*time.Second, 10*time.Millisecond).
			Should(Equal(0))

		target.send("check")
		Expect(target.line(5 * time.Second)).To(Equal("BYTES_OK"))
		target.send("run")
		Expect(target.line(5 * time.Second)).To(Equal("RAN"))
		target.send("exit")
		Expect(target.line(5 * time.Second)).To(Equal("DONE"))
		select {
		case err := <-target.wait:
			Expect(err).NotTo(HaveOccurred(), "control target exit; stderr=%q", target.stderr.String())
		case <-time.After(5 * time.Second):
			Fail("control target did not exit after detach")
		}
	})
}
