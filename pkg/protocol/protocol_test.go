package protocol_test

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/pkg/protocol"
)

func TestProtocol(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Protocol Suite")
}

var sampleLocation = protocol.Location{
	File:     "main.go",
	Line:     42,
	Function: "main.main",
}

var sampleBreakpoint = protocol.Breakpoint{
	ID:       1,
	Enabled:  true,
	Location: sampleLocation,
}

var sampleGoroutine = protocol.Goroutine{
	ID:         1,
	Status:     "waiting",
	CurrentLoc: sampleLocation,
	GoLoc:      protocol.Location{File: "runtime/proc.go", Line: 10},
}

var sampleFrames = []protocol.Frame{
	{Index: 0, Location: sampleLocation},
	{Index: 1, Location: protocol.Location{File: "runtime/proc.go", Line: 271, Function: "runtime.main"}},
}

var sampleVariables = []protocol.Variable{
	{Name: "x", Value: "0x2a", Type: "int"},
	{Name: "msg", Value: "0xc000014070", Type: "string"},
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

var _ = Describe("Event", func() {

	Describe("NewEvent", func() {
		Context("with a valid payload", func() {
			var event protocol.Event

			BeforeEach(func() {
				var err error
				event, err = protocol.NewEvent(protocol.EventBreakpointHit, 7,
					protocol.BreakpointHitPayload{
						Breakpoint: sampleBreakpoint,
						Goroutine:  sampleGoroutine,
						Frames:     sampleFrames,
					})
				Expect(err).NotTo(HaveOccurred())
			})

			It("stamps the current protocol version", func() {
				Expect(event.Version).To(Equal(protocol.Version))
			})
			It("preserves the kind", func() {
				Expect(event.Kind).To(Equal(protocol.EventBreakpointHit))
			})
			It("preserves the sequence number", func() {
				Expect(event.Seq).To(Equal(uint64(7)))
			})
			It("produces a non-empty payload", func() {
				Expect(event.Payload).NotTo(BeEmpty())
			})
		})

		Context("with an un-marshallable payload", func() {
			It("returns an error", func() {
				_, err := protocol.NewEvent(protocol.EventError, 0, make(chan int))
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("MustEvent", func() {
		It("panics on un-marshallable payload", func() {
			Expect(func() {
				protocol.MustEvent(protocol.EventError, 0, make(chan int))
			}).To(Panic())
		})
		It("does not panic on valid payload", func() {
			Expect(func() {
				protocol.MustEvent(protocol.EventContinued, 1, protocol.ContinuedPayload{})
			}).NotTo(Panic())
		})
	})

	Describe("wire round-trip", func() {
		DescribeTable("all event payload types survive marshal → unmarshal",
			func(kind protocol.EventKind, payload any, decode func(protocol.Event)) {
				event, err := protocol.NewEvent(kind, 1, payload)
				Expect(err).NotTo(HaveOccurred())

				wire, err := protocol.MarshalEvent(event)
				Expect(err).NotTo(HaveOccurred())

				decoded, err := protocol.UnmarshalEvent(wire)
				Expect(err).NotTo(HaveOccurred())
				Expect(decoded.Kind).To(Equal(kind))
				Expect(decoded.Version).To(Equal(protocol.Version))
				Expect(decoded.Seq).To(Equal(uint64(1)))

				decode(decoded)
			},

			Entry("BreakpointHit",
				protocol.EventBreakpointHit,
				protocol.BreakpointHitPayload{
					Breakpoint: sampleBreakpoint,
					Goroutine:  sampleGoroutine,
					Frames:     sampleFrames,
				},
				func(e protocol.Event) {
					var p protocol.BreakpointHitPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.Breakpoint.ID).To(Equal(1))
					Expect(p.Breakpoint.Location.File).To(Equal("main.go"))
					Expect(p.Goroutine.Status).To(Equal("waiting"))
					Expect(p.Frames).To(HaveLen(2))
				},
			),

			Entry("Panic",
				protocol.EventPanic,
				protocol.PanicPayload{
					Message:   "runtime error: index out of range",
					Goroutine: sampleGoroutine,
					Frames:    sampleFrames,
				},
				func(e protocol.Event) {
					var p protocol.PanicPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.Message).To(ContainSubstring("index out of range"))
					Expect(p.Frames).To(HaveLen(2))
				},
			),

			Entry("Output",
				protocol.EventOutput,
				protocol.OutputPayload{Stream: "stdout", Content: "hello bingo\n"},
				func(e protocol.Event) {
					var p protocol.OutputPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.Stream).To(Equal("stdout"))
					Expect(p.Content).To(Equal("hello bingo\n"))
				},
			),

			Entry("ProcessExited",
				protocol.EventProcessExited,
				protocol.ProcessExitedPayload{ExitCode: 0},
				func(e protocol.Event) {
					var p protocol.ProcessExitedPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.ExitCode).To(Equal(0))
				},
			),

			Entry("ProcessExited with non-zero code",
				protocol.EventProcessExited,
				protocol.ProcessExitedPayload{ExitCode: 2, Reason: "killed"},
				func(e protocol.Event) {
					var p protocol.ProcessExitedPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.ExitCode).To(Equal(2))
					Expect(p.Reason).To(Equal("killed"))
				},
			),

			Entry("BreakpointSet",
				protocol.EventBreakpointSet,
				protocol.BreakpointSetPayload{Breakpoint: sampleBreakpoint},
				func(e protocol.Event) {
					var p protocol.BreakpointSetPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.Breakpoint.ID).To(Equal(1))
					Expect(p.Breakpoint.Enabled).To(BeTrue())
				},
			),

			Entry("BreakpointCleared",
				protocol.EventBreakpointCleared,
				protocol.BreakpointClearedPayload{ID: 3},
				func(e protocol.Event) {
					var p protocol.BreakpointClearedPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.ID).To(Equal(3))
				},
			),

			Entry("Stepped",
				protocol.EventStepped,
				protocol.SteppedPayload{
					Goroutine: sampleGoroutine,
					Location:  sampleLocation,
					Frames:    sampleFrames,
				},
				func(e protocol.Event) {
					var p protocol.SteppedPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.Location.Line).To(Equal(42))
					Expect(p.Frames).To(HaveLen(2))
				},
			),

			Entry("Continued",
				protocol.EventContinued,
				protocol.ContinuedPayload{},
				func(e protocol.Event) {
					var p protocol.ContinuedPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
				},
			),

			Entry("Locals",
				protocol.EventLocals,
				protocol.LocalsPayload{FrameIndex: 0, Variables: sampleVariables},
				func(e protocol.Event) {
					var p protocol.LocalsPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.FrameIndex).To(Equal(0))
					Expect(p.Variables).To(HaveLen(2))
					Expect(p.Variables[0].Name).To(Equal("x"))
					Expect(p.Variables[0].Type).To(Equal("int"))
				},
			),

			Entry("Frames",
				protocol.EventFrames,
				protocol.FramesPayload{Frames: sampleFrames},
				func(e protocol.Event) {
					var p protocol.FramesPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.Frames).To(HaveLen(2))
					Expect(p.Frames[0].Index).To(Equal(0))
				},
			),

			Entry("Goroutines",
				protocol.EventGoroutines,
				protocol.GoroutinesPayload{Goroutines: []protocol.Goroutine{sampleGoroutine}},
				func(e protocol.Event) {
					var p protocol.GoroutinesPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.Goroutines).To(HaveLen(1))
					Expect(p.Goroutines[0].ID).To(Equal(1))
				},
			),

			Entry("Error with command",
				protocol.EventError,
				protocol.ErrorPayload{Command: protocol.CmdSetBreakpoint, Message: "address not found"},
				func(e protocol.Event) {
					var p protocol.ErrorPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.Command).To(Equal(protocol.CmdSetBreakpoint))
					Expect(p.Message).To(Equal("address not found"))
				},
			),

			Entry("Error with CmdNone omits command field on wire",
				protocol.EventError,
				protocol.ErrorPayload{Command: protocol.CmdNone, Message: "backend failure"},
				func(e protocol.Event) {
					var p protocol.ErrorPayload
					Expect(protocol.DecodeEventPayload(e, &p)).To(Succeed())
					Expect(p.Command).To(Equal(protocol.CmdNone))
					Expect(p.Message).To(Equal("backend failure"))
					var raw map[string]any
					Expect(json.Unmarshal(e.Payload, &raw)).To(Succeed())
					_, hasCmd := raw["command"]
					Expect(hasCmd).To(BeFalse(), "command field should be omitted for CmdNone")
				},
			),
		)
	})

	Describe("UnmarshalEvent", func() {
		It("returns an error for malformed JSON", func() {
			_, err := protocol.UnmarshalEvent([]byte("not json {{"))
			Expect(err).To(HaveOccurred())
		})
		It("returns an error for an empty byte slice", func() {
			_, err := protocol.UnmarshalEvent([]byte{})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("DecodeEventPayload", func() {
		It("does not panic when decoding into a mismatched struct", func() {
			event, err := protocol.NewEvent(protocol.EventError, 1, protocol.ErrorPayload{
				Message: "something went wrong",
			})
			Expect(err).NotTo(HaveOccurred())
			var wrong protocol.BreakpointHitPayload
			Expect(func() {
				_ = protocol.DecodeEventPayload(event, &wrong)
			}).NotTo(Panic())
		})
		It("returns an error when payload is not valid JSON for the target type", func() {
			event := protocol.Event{
				Version: protocol.Version,
				Kind:    protocol.EventBreakpointHit,
				Seq:     1,
				Payload: json.RawMessage(`[1,2,3]`),
			}
			var p protocol.BreakpointHitPayload
			err := protocol.DecodeEventPayload(event, &p)
			Expect(err).To(HaveOccurred())
		})
	})
})

var _ = Describe("Command", func() {

	Describe("wire round-trip", func() {
		DescribeTable("all command payload types survive marshal → unmarshal",
			func(kind protocol.CommandKind, payload any, decode func(protocol.Command)) {
				cmd := protocol.Command{
					Version: protocol.Version,
					Kind:    kind,
					Payload: mustRaw(payload),
				}
				wire, err := json.Marshal(cmd)
				Expect(err).NotTo(HaveOccurred())

				decoded, err := protocol.UnmarshalCommand(wire)
				Expect(err).NotTo(HaveOccurred())
				Expect(decoded.Kind).To(Equal(kind))
				Expect(decoded.Version).To(Equal(protocol.Version))

				decode(decoded)
			},

			Entry("Launch",
				protocol.CmdLaunch,
				protocol.LaunchPayload{Program: "/tmp/myapp", Args: []string{"--verbose"}},
				func(c protocol.Command) {
					var p protocol.LaunchPayload
					Expect(protocol.DecodeCommandPayload(c, &p)).To(Succeed())
					Expect(p.Program).To(Equal("/tmp/myapp"))
					Expect(p.Args).To(ConsistOf("--verbose"))
				},
			),

			Entry("Attach",
				protocol.CmdAttach,
				protocol.AttachPayload{PID: 12345},
				func(c protocol.Command) {
					var p protocol.AttachPayload
					Expect(protocol.DecodeCommandPayload(c, &p)).To(Succeed())
					Expect(p.PID).To(Equal(12345))
				},
			),

			Entry("Kill",
				protocol.CmdKill,
				json.RawMessage(`{}`),
				func(c protocol.Command) {
					Expect(c.Kind).To(Equal(protocol.CmdKill))
				},
			),

			Entry("SetBreakpoint",
				protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "server.go", Line: 100},
				func(c protocol.Command) {
					var p protocol.SetBreakpointPayload
					Expect(protocol.DecodeCommandPayload(c, &p)).To(Succeed())
					Expect(p.File).To(Equal("server.go"))
					Expect(p.Line).To(Equal(100))
				},
			),

			Entry("ClearBreakpoint",
				protocol.CmdClearBreakpoint,
				protocol.ClearBreakpointPayload{ID: 7},
				func(c protocol.Command) {
					var p protocol.ClearBreakpointPayload
					Expect(protocol.DecodeCommandPayload(c, &p)).To(Succeed())
					Expect(p.ID).To(Equal(7))
				},
			),

			Entry("Continue",
				protocol.CmdContinue,
				json.RawMessage(`{}`),
				func(c protocol.Command) {
					Expect(c.Kind).To(Equal(protocol.CmdContinue))
				},
			),

			Entry("StepOver",
				protocol.CmdStepOver,
				json.RawMessage(`{}`),
				func(c protocol.Command) {
					Expect(c.Kind).To(Equal(protocol.CmdStepOver))
				},
			),

			Entry("StepInto",
				protocol.CmdStepInto,
				json.RawMessage(`{}`),
				func(c protocol.Command) {
					Expect(c.Kind).To(Equal(protocol.CmdStepInto))
				},
			),

			Entry("StepOut",
				protocol.CmdStepOut,
				json.RawMessage(`{}`),
				func(c protocol.Command) {
					Expect(c.Kind).To(Equal(protocol.CmdStepOut))
				},
			),

			Entry("Locals",
				protocol.CmdLocals,
				protocol.LocalsPayloadCmd{FrameIndex: 2},
				func(c protocol.Command) {
					var p protocol.LocalsPayloadCmd
					Expect(protocol.DecodeCommandPayload(c, &p)).To(Succeed())
					Expect(p.FrameIndex).To(Equal(2))
				},
			),

			Entry("Frames",
				protocol.CmdFrames,
				json.RawMessage(`{}`),
				func(c protocol.Command) {
					Expect(c.Kind).To(Equal(protocol.CmdFrames))
				},
			),

			Entry("Goroutines",
				protocol.CmdGoroutines,
				json.RawMessage(`{}`),
				func(c protocol.Command) {
					Expect(c.Kind).To(Equal(protocol.CmdGoroutines))
				},
			),
		)
	})

	Describe("DecodeCommandPayload", func() {
		It("returns an error for malformed payload JSON", func() {
			cmd := protocol.Command{
				Version: protocol.Version,
				Kind:    protocol.CmdSetBreakpoint,
				Payload: json.RawMessage(`not-json`),
			}
			var p protocol.SetBreakpointPayload
			Expect(protocol.DecodeCommandPayload(cmd, &p)).To(HaveOccurred())
		})
	})

	Describe("UnmarshalCommand", func() {
		It("returns an error for malformed JSON", func() {
			_, err := protocol.UnmarshalCommand([]byte("not json"))
			Expect(err).To(HaveOccurred())
		})
		It("returns an error for an empty byte slice", func() {
			_, err := protocol.UnmarshalCommand([]byte{})
			Expect(err).To(HaveOccurred())
		})
	})
})

var _ = Describe("Kind constants", func() {

	It("all non-sentinel EventKind values are non-empty strings", func() {
		kinds := []protocol.EventKind{
			protocol.EventBreakpointHit,
			protocol.EventPanic,
			protocol.EventOutput,
			protocol.EventProcessExited,
			protocol.EventBreakpointSet,
			protocol.EventBreakpointCleared,
			protocol.EventStepped,
			protocol.EventContinued,
			protocol.EventLocals,
			protocol.EventFrames,
			protocol.EventGoroutines,
			protocol.EventError,
		}
		for _, k := range kinds {
			Expect(string(k)).NotTo(BeEmpty(), "EventKind %q should not be empty", k)
		}
	})

	It("all non-sentinel CommandKind values are non-empty strings", func() {
		kinds := []protocol.CommandKind{
			protocol.CmdLaunch,
			protocol.CmdAttach,
			protocol.CmdKill,
			protocol.CmdSetBreakpoint,
			protocol.CmdClearBreakpoint,
			protocol.CmdContinue,
			protocol.CmdStepOver,
			protocol.CmdStepInto,
			protocol.CmdStepOut,
			protocol.CmdLocals,
			protocol.CmdFrames,
			protocol.CmdGoroutines,
		}
		for _, k := range kinds {
			Expect(string(k)).NotTo(BeEmpty(), "CommandKind %q should not be empty", k)
		}
	})

	It("CmdNone is the empty string (intentional sentinel)", func() {
		Expect(string(protocol.CmdNone)).To(BeEmpty())
	})
})

var _ = Describe("Sequence numbers", func() {
	It("are preserved exactly through marshal/unmarshal", func() {
		for _, seq := range []uint64{0, 1, 255, 1<<32 - 1, 1<<63 - 1} {
			e, err := protocol.NewEvent(protocol.EventOutput, seq,
				protocol.OutputPayload{Stream: "stdout", Content: "x"})
			Expect(err).NotTo(HaveOccurred())
			wire, err := protocol.MarshalEvent(e)
			Expect(err).NotTo(HaveOccurred())
			decoded, err := protocol.UnmarshalEvent(wire)
			Expect(err).NotTo(HaveOccurred())
			Expect(decoded.Seq).To(Equal(seq), "seq %d should survive wire round-trip", seq)
		}
	})
})

var _ = Describe("Version", func() {
	It("is non-empty", func() {
		Expect(protocol.Version).NotTo(BeEmpty())
	})
	It("is stamped on every event", func() {
		e, err := protocol.NewEvent(protocol.EventOutput, 1, protocol.OutputPayload{Content: "x"})
		Expect(err).NotTo(HaveOccurred())
		wire, _ := protocol.MarshalEvent(e)
		decoded, _ := protocol.UnmarshalEvent(wire)
		Expect(decoded.Version).To(Equal(protocol.Version))
	})
})
