package debugger

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDebugger(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Debugger Suite")
}

var _ = Describe("Debugger", func() {
	Describe("NewDebugger", func() {
		It("should initialize all channels with expected buffer sizes and default map state", func() {
			d := NewDebugger()

			Expect(d.Breakpoints).NotTo(BeNil())
			Expect(d.Breakpoints).To(BeEmpty())

			Expect(cap(d.EndDebugSession)).To(Equal(1))
			Expect(cap(d.BreakpointHit)).To(Equal(1))
			Expect(cap(d.InitialBreakpointHit)).To(Equal(1))
			Expect(cap(d.DebugCommand)).To(Equal(1))
		})

		It("should start with empty breakpoint storage and zero-value debug info", func() {
			d := NewDebugger()

			Expect(d.DebugInfo.Target.PID).To(Equal(0))
			Expect(d.DebugInfo.Target.PGID).To(Equal(0))
			Expect(d.DebugInfo.Target.Path).To(BeEmpty())
		})
	})

	Describe("validateTargetPath", func() {
		It("should return absolute cleaned path for executable target inside current working directory", func() {
			cwd, err := os.Getwd()
			Expect(err).NotTo(HaveOccurred())

			file, err := os.CreateTemp(cwd, "debugger-exec-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				Expect(os.Remove(file.Name())).To(Succeed())
			}()

			_, err = file.WriteString("#!/bin/sh\nexit 0\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(file.Close()).To(Succeed())
			Expect(os.Chmod(file.Name(), 0o755)).To(Succeed())

			validated, err := validateTargetPath(file.Name())
			Expect(err).NotTo(HaveOccurred())

			expected, err := filepath.Abs(filepath.Clean(file.Name()))
			Expect(err).NotTo(HaveOccurred())
			Expect(validated).To(Equal(expected))
		})

		It("should reject paths outside working directory to prevent traversal and command injection", func() {
			outsideDir, err := os.MkdirTemp("", "debugger-outside-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				Expect(os.RemoveAll(outsideDir)).To(Succeed())
			}()

			outsideFile := filepath.Join(outsideDir, "target")
			Expect(os.WriteFile(outsideFile, []byte("#!/bin/sh\nexit 0\n"), 0o755)).To(Succeed())

			_, err = validateTargetPath(outsideFile)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("outside the working directory"))
		})

		It("should reject non-regular files such as directories or device files", func() {
			cwd, err := os.Getwd()
			Expect(err).NotTo(HaveOccurred())

			dir, err := os.MkdirTemp(cwd, "debugger-dir-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				Expect(os.RemoveAll(dir)).To(Succeed())
			}()

			_, err = validateTargetPath(dir)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not a regular file"))
		})

		It("should reject files without execute permissions", func() {
			cwd, err := os.Getwd()
			Expect(err).NotTo(HaveOccurred())

			file, err := os.CreateTemp(cwd, "debugger-noexec-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				Expect(os.Remove(file.Name())).To(Succeed())
			}()

			Expect(file.Close()).To(Succeed())
			Expect(os.Chmod(file.Name(), 0o644)).To(Succeed())

			_, err = validateTargetPath(file.Name())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not executable"))
		})

		It("should return descriptive errors when path cannot be resolved or accessed", func() {
			_, err := validateTargetPath("./definitely-not-there-binary")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not accessible"))
		})
	})

	Describe("StartWithDebug", func() {
		It("should validate user path before creating target process", func() {
			d := NewDebugger()
			outsideDir, err := os.MkdirTemp("", "debugger-outside-start-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				Expect(os.RemoveAll(outsideDir)).To(Succeed())
			}()

			outsideFile := filepath.Join(outsideDir, "target")
			Expect(os.WriteFile(outsideFile, []byte("#!/bin/sh\nexit 0\n"), 0o755)).To(Succeed())

			Expect(func() {
				d.StartWithDebug(outsideFile)
			}).To(Panic())
		})

		PIt("TODO: should configure ptrace options, create debug info, and enter initial breakpoint flow")
		PIt("TODO: should end early when initial breakpoint phase signals end of debug session")
		PIt("TODO: should continue into main debug loop after initial setup completes")

		It("should panic on target start or debug info initialization failures", func() {
			d := NewDebugger()
			cwd, err := os.Getwd()
			Expect(err).NotTo(HaveOccurred())

			file, err := os.CreateTemp(cwd, "debugger-nonexec-start-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				Expect(os.Remove(file.Name())).To(Succeed())
			}()

			Expect(file.Close()).To(Succeed())
			Expect(os.Chmod(file.Name(), 0o644)).To(Succeed())

			Expect(func() {
				d.StartWithDebug(file.Name())
			}).To(Panic())
		})
	})

	Describe("Continue", func() {
		PIt("TODO: should rewind RIP after breakpoint trap and clear/reapply breakpoint around single-step")
		PIt("TODO: should call ptrace continue after stepping over restored instruction")
		PIt("TODO: should panic when register operations or ptrace operations fail")
	})

	Describe("SingleStep", func() {
		PIt("TODO: should execute one ptrace single-step for the provided PID")
		PIt("TODO: should panic when ptrace single-step fails")
	})

	Describe("StopDebug", func() {
		PIt("TODO: should detach from target when PID is available")

		It("should signal EndDebugSession without blocking when channel already contains a signal", func() {
			d := NewDebugger()
			d.EndDebugSession <- true

			done := make(chan struct{})
			go func() {
				d.StopDebug()
				close(done)
			}()

			Eventually(done, time.Second).Should(BeClosed())
		})

		It("should skip detach and still signal end when target PID is not set", func() {
			d := NewDebugger()
			d.DebugInfo.Target.PID = 0

			d.StopDebug()

			Eventually(d.EndDebugSession, time.Second).Should(Receive(BeTrue()))
		})
	})

	Describe("SetBreakpoint", func() {
		PIt("TODO: should resolve source line to PC, read original bytes, write breakpoint byte, and store original instruction")
		PIt("TODO: should return error when source line cannot be mapped to a program counter")
		PIt("TODO: should return error when ptrace peek or poke fails")
	})

	Describe("ClearBreakpoint", func() {
		PIt("TODO: should resolve source line to PC and restore original machine code at breakpoint address")
		PIt("TODO: should return error when source line cannot be mapped to a program counter")
		PIt("TODO: should return error when restoring original code fails")
	})

	Describe("mainDebugLoop", func() {
		It("should exit immediately when EndDebugSession is signaled", func() {
			d := NewDebugger()
			d.EndDebugSession <- true

			done := make(chan struct{})
			go func() {
				d.mainDebugLoop()
				close(done)
			}()

			Eventually(done, time.Second).Should(BeClosed())
		})

		PIt("TODO: should emit EndDebugSession when main target process exits")
		PIt("TODO: should ignore idle wait states with short sleep when no child state changes")
		PIt("TODO: should route SIGTRAP breakpoint stops to breakpointHit and continue non-breakpoint stops")

		It("should return gracefully on wait failures", func() {
			d := NewDebugger()
			d.DebugInfo.Target.PGID = -12345

			done := make(chan struct{})
			go func() {
				d.mainDebugLoop()
				close(done)
			}()

			Eventually(done, time.Second).Should(BeClosed())
		})
	})

	Describe("initialBreakpointHit", func() {
		It("should emit initial breakpoint event with target PID and await incoming debug commands", func() {
			d := NewDebugger()
			d.DebugInfo.Target.PID = 99

			done := make(chan struct{})
			go func() {
				d.initialBreakpointHit()
				close(done)
			}()

			Eventually(d.InitialBreakpointHit, time.Second).Should(Receive(Equal(InitialBreakpointHitEvent{PID: 99})))

			d.EndDebugSession <- true
			Eventually(done, time.Second).Should(BeClosed())
		})

		It("should ignore malformed setBreakpoint payloads during initial stop and keep waiting", func() {
			d := NewDebugger()
			d.DebugInfo.Target.PID = 7

			done := make(chan struct{})
			go func() {
				d.initialBreakpointHit()
				close(done)
			}()

			Eventually(d.InitialBreakpointHit, time.Second).Should(Receive(Equal(InitialBreakpointHitEvent{PID: 7})))
			d.DebugCommand <- DebugCommand{Type: "setBreakpoint", Data: "bad-payload"}

			Consistently(done, 200*time.Millisecond).ShouldNot(BeClosed())

			d.EndDebugSession <- true
			Eventually(done, time.Second).Should(BeClosed())
		})

		PIt("TODO: should continue traced process on continue command and exit initial loop")

		It("should stop debug session on quit command", func() {
			d := NewDebugger()
			d.DebugInfo.Target.PID = 0

			done := make(chan struct{})
			go func() {
				d.initialBreakpointHit()
				close(done)
			}()

			Eventually(d.InitialBreakpointHit, time.Second).Should(Receive(Equal(InitialBreakpointHitEvent{PID: 0})))
			d.DebugCommand <- DebugCommand{Type: "quit"}

			Eventually(done, time.Second).Should(BeClosed())
			Eventually(d.EndDebugSession, time.Second).Should(Receive(BeTrue()))
		})

		It("should exit when EndDebugSession signal arrives", func() {
			d := NewDebugger()
			d.DebugInfo.Target.PID = 101

			done := make(chan struct{})
			go func() {
				d.initialBreakpointHit()
				close(done)
			}()

			Eventually(d.InitialBreakpointHit, time.Second).Should(Receive(Equal(InitialBreakpointHitEvent{PID: 101})))
			d.EndDebugSession <- true

			Eventually(done, time.Second).Should(BeClosed())
		})

		It("should ignore unknown commands and continue waiting for a valid command or end signal", func() {
			d := NewDebugger()
			d.DebugInfo.Target.PID = 202

			done := make(chan struct{})
			go func() {
				d.initialBreakpointHit()
				close(done)
			}()

			Eventually(d.InitialBreakpointHit, time.Second).Should(Receive(Equal(InitialBreakpointHitEvent{PID: 202})))
			d.DebugCommand <- DebugCommand{Type: "unknown"}

			Consistently(done, 200*time.Millisecond).ShouldNot(BeClosed())

			d.EndDebugSession <- true
			Eventually(done, time.Second).Should(BeClosed())
		})
	})

	Describe("breakpointHit", func() {
		PIt("TODO: should read registers, translate PC to source location, and emit breakpoint event")
		PIt("TODO: should dispatch continue command to Continue for current PID")
		PIt("TODO: should dispatch step command to SingleStep for current PID")
		PIt("TODO: should process setBreakpoint command payload and attempt new breakpoint placement")
		PIt("TODO: should stop debug session on quit command and return")
		PIt("TODO: should default to continue for unknown commands")
		PIt("TODO: should return when EndDebugSession is signaled while waiting for commands")
	})

	Describe("Testify Mock Integration", func() {
		PIt("TODO: should introduce seam interfaces for ptrace/process operations and verify interactions with testify mock")
		PIt("TODO: should assert command-handling behavior using mocked dependencies instead of real syscalls")
	})
})
