//go:build darwin && arm64 && cgo

package debugger

import (
	"os"
	"path/filepath"
	"time"

	"github.com/bingosuite/bingo/internal/debuginfo"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func newTestDebugger() *darwinARM64Debugger {
	breakpointHit := make(chan BreakpointEvent, 1)
	initialBreakpointHit := make(chan InitialBreakpointHitEvent, 1)
	debugCommand := make(chan DebugCommand, 1)
	endDebugSession := make(chan bool, 1)

	dbg := NewDebugger(breakpointHit, initialBreakpointHit, debugCommand, endDebugSession)
	typed, ok := dbg.(*darwinARM64Debugger)
	if !ok {
		panic("expected darwinARM64Debugger")
	}
	return typed
}

var _ = Describe("Debugger Darwin ARM64", func() {
	Describe("NewDebugger", func() {
		It("should initialize all channels with expected buffer sizes and default map state", func() {
			d := newTestDebugger()

			Expect(d.Breakpoints).NotTo(BeNil())
			Expect(d.Breakpoints).To(BeEmpty())

			Expect(cap(d.EndDebugSession)).To(Equal(1))
			Expect(cap(d.BreakpointHit)).To(Equal(1))
			Expect(cap(d.InitialBreakpointHit)).To(Equal(1))
			Expect(cap(d.DebugCommand)).To(Equal(1))
		})

		It("should start with empty breakpoint storage and zero-value debug info", func() {
			d := newTestDebugger()
			Expect(d.DebugInfo).To(BeNil())
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
	})

	Describe("StartWithDebug", func() {
		It("should validate user path before creating target process", func() {
			d := newTestDebugger()
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

		It("should panic on target start failures for non-executable files", func() {
			d := newTestDebugger()
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
		It("should panic when register read fails", func() {
			d := newTestDebugger()
			Expect(func() {
				d.Continue(-1)
			}).To(Panic())
		})
	})

	Describe("SingleStep", func() {
		It("should panic when register or single-step setup fails", func() {
			d := newTestDebugger()
			Expect(func() {
				d.SingleStep(-1)
			}).To(Panic())
		})
	})

	Describe("StopDebug", func() {
		It("should signal EndDebugSession without blocking when channel already contains a signal", func() {
			d := newTestDebugger()
			d.EndDebugSession <- true

			done := make(chan struct{})
			go func() {
				d.StopDebug()
				close(done)
			}()

			Eventually(done, time.Second).Should(BeClosed())
		})

		It("should signal end when target PID is not set", func() {
			d := newTestDebugger()
			d.DebugInfo = &testDebugInfo{target: debuginfo.Target{PID: 0}}

			d.StopDebug()

			Eventually(d.EndDebugSession, time.Second).Should(Receive(BeTrue()))
		})
	})

	Describe("SetBreakpoint", func() {
		It("should return error when debug info is not initialized", func() {
			d := newTestDebugger()
			err := d.SetBreakpoint(1, 1)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("debug info not initialized"))
		})

		It("should return error when debugger task is not initialized", func() {
			d := newTestDebugger()
			d.DebugInfo = &testDebugInfo{target: debuginfo.Target{Path: "/tmp/main.go", PID: 1}}

			err := d.SetBreakpoint(1, 1)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("task is not initialized"))
		})
	})

	Describe("ClearBreakpoint", func() {
		It("should return error when source line cannot be mapped to a program counter", func() {
			d := newTestDebugger()
			d.DebugInfo = &testDebugInfo{
				target:      debuginfo.Target{Path: "/nonexistent/path.go", PID: os.Getpid()},
				lineToPCErr: os.ErrNotExist,
			}

			err := d.ClearBreakpoint(os.Getpid(), 1)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("initialBreakpointHit", func() {
		It("should emit initial breakpoint event with target PID and await incoming debug commands", func() {
			d := newTestDebugger()
			d.DebugInfo = &testDebugInfo{target: debuginfo.Target{PID: 99}}

			done := make(chan struct{})
			go func() {
				d.initialBreakpointHit()
				close(done)
			}()

			Eventually(d.InitialBreakpointHit, time.Second).Should(Receive(Equal(InitialBreakpointHitEvent{PID: 99})))
			d.EndDebugSession <- true
			Eventually(done, time.Second).Should(BeClosed())
		})

		It("should ignore unknown commands and continue waiting for valid command or end signal", func() {
			d := newTestDebugger()
			d.DebugInfo = &testDebugInfo{target: debuginfo.Target{PID: 202}}

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
		It("should panic when register read fails", func() {
			d := newTestDebugger()
			d.DebugInfo = &testDebugInfo{target: debuginfo.Target{PID: 1}}
			Expect(func() {
				d.breakpointHit(1)
			}).To(Panic())
		})
	})
})
