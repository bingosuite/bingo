package debuginfo

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDebugInfo(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DebugInfo Suite")
}

var _ = Describe("DebugInfo", func() {
	Describe("Helper methods", func() {
		It("should return nil for unknown function lookups", func() {
			exePath, err := os.Executable()
			Expect(err).NotTo(HaveOccurred())

			di, err := NewDebugInfo(exePath, os.Getpid())
			Expect(err).NotTo(HaveOccurred())

			fn := di.LookupFunc("this.function.does.not.exist")
			Expect(fn).To(BeNil())
		})

		It("should fail to map unknown file and line to a program counter", func() {
			exePath, err := os.Executable()
			Expect(err).NotTo(HaveOccurred())

			di, err := NewDebugInfo(exePath, os.Getpid())
			Expect(err).NotTo(HaveOccurred())

			pc, fn, err := di.LineToPC("/tmp/does-not-exist.go", 1)
			Expect(err).To(HaveOccurred())
			Expect(pc).To(Equal(uint64(0)))
			Expect(fn).To(BeNil())
		})

		It("should return zero values for an unmapped program counter", func() {
			exePath, err := os.Executable()
			Expect(err).NotTo(HaveOccurred())

			di, err := NewDebugInfo(exePath, os.Getpid())
			Expect(err).NotTo(HaveOccurred())

			file, line, pcFn := di.PCToLine(0)
			Expect(file).To(Equal(""))
			Expect(line).To(Equal(0))
			Expect(pcFn).To(BeNil())
		})
	})
})
