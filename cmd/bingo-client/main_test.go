package main

import (
	"testing"

	"github.com/bingosuite/bingo/pkg/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestBingoClientMain(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Bingo Client Main Suite")
}

var _ = Describe("Command handlers", func() {
	var c *client.Client

	BeforeEach(func() {
		c = client.NewClient("localhost:8080", "session-1")
	})

	Describe("handleBreakpointCommand", func() {
		It("returns nil for empty input", func() {
			err := handleBreakpointCommand(c, "   ")
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns unknown command for unsupported command names", func() {
			err := handleBreakpointCommand(c, "foo 10")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("unknown command"))
		})

		It("returns usage error when arg count is invalid", func() {
			err := handleBreakpointCommand(c, "b")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("usage: b <line> or b <file> <line>"))

			err = handleBreakpointCommand(c, "b a b c")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("usage: b <line> or b <file> <line>"))
		})

		It("returns invalid line number for non-numeric line", func() {
			err := handleBreakpointCommand(c, "b not-a-number")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("invalid line number"))
		})

		It("returns invalid line number for zero or negative lines", func() {
			err := handleBreakpointCommand(c, "b 0")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("invalid line number"))

			err = handleBreakpointCommand(c, "b -3")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("invalid line number"))
		})

		It("accepts line-only breakpoint command", func() {
			err := handleBreakpointCommand(c, "b 42")
			Expect(err).NotTo(HaveOccurred())
		})

		It("accepts file and line breakpoint command", func() {
			err := handleBreakpointCommand(c, "breakpoint main.go 27")
			Expect(err).NotTo(HaveOccurred())
		})

		It("accepts command aliases", func() {
			Expect(handleBreakpointCommand(c, "break 18")).To(Succeed())
			Expect(handleBreakpointCommand(c, "B 19")).To(Succeed())
		})
	})

	Describe("handleStartCommand", func() {
		It("returns usage error when arg count is invalid", func() {
			err := handleStartCommand(c, []string{"start"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("usage: start <path>"))

			err = handleStartCommand(c, []string{"start", "./bin/target", "extra"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("usage: start <path>"))
		})

		It("accepts a valid start command", func() {
			err := handleStartCommand(c, []string{"start", "./bin/target"})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
