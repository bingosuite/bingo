package debuginfo

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/sys/unix"
)

func TestDebugInfo(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DebugInfo Suite")
}

var _ = Describe("DebugInfo", func() {
	Describe("NewDebugInfo", func() {
		It("should initialize symbol and line table data for a valid executable and PID", func() {
			exePath, err := os.Executable()
			Expect(err).NotTo(HaveOccurred())

			pid := os.Getpid()
			di, err := NewDebugInfo(exePath, pid)
			Expect(err).NotTo(HaveOccurred())

			Expect(di).NotTo(BeNil())
			Expect(di.SymTable).NotTo(BeNil())
			Expect(di.LineTable).NotTo(BeNil())
			Expect(di.Target.PID).To(Equal(pid))

			expectedPGID, err := unix.Getpgid(pid)
			Expect(err).NotTo(HaveOccurred())
			Expect(di.Target.PGID).To(Equal(expectedPGID))
			Expect(di.Target.Path).NotTo(BeEmpty())
		})

		It("should return an error when executable path does not exist", func() {
			_, err := NewDebugInfo("/definitely/does/not/exist", os.Getpid())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to open target ELF file"))
		})

		It("should return an error when PID has no process group", func() {
			exePath, err := os.Executable()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewDebugInfo(exePath, -1)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("error getting PGID"))
		})
	})

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
