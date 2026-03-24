//go:build darwin

package debuginfo

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/sys/unix"
)

var _ = Describe("DebugInfo Darwin", func() {
	Describe("NewDebugInfo", func() {
		It("should initialize symbol and line table data for a valid executable and PID", func() {
			exePath, err := os.Executable()
			Expect(err).NotTo(HaveOccurred())

			pid := os.Getpid()
			di, err := NewDebugInfo(exePath, pid)
			Expect(err).NotTo(HaveOccurred())

			Expect(di).NotTo(BeNil())
			Expect(di.LookupFunc("main.main")).NotTo(BeNil())

			target := di.GetTarget()
			Expect(target.PID).To(Equal(pid))

			expectedPGID, err := unix.Getpgid(pid)
			Expect(err).NotTo(HaveOccurred())
			Expect(target.PGID).To(Equal(expectedPGID))
			Expect(target.Path).NotTo(BeEmpty())
		})

		It("should return an error when executable path does not exist", func() {
			_, err := NewDebugInfo("/definitely/does/not/exist", os.Getpid())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to open target Mach-O file"))
		})

		It("should return an error when PID has no process group", func() {
			exePath, err := os.Executable()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewDebugInfo(exePath, -1)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("error getting PGID"))
		})
	})
})
