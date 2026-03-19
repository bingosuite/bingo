package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCommitLint(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "CommitLint Suite")
}

var _ = Describe("commitlint", func() {
	runCommitlint := func(messageFile string) (string, error) {
		cmd := exec.Command("go", "run", ".", messageFile)
		cmd.Dir = "/workspaces/bingo/cmd/githook"

		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	writeMessage := func(content string) string {
		path := filepath.Join(GinkgoT().TempDir(), "COMMIT_EDITMSG")
		Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
		return path
	}

	It("accepts valid conventional commit messages", func() {
		messagePath := writeMessage("feat(parser): add array parsing\n")

		output, err := runCommitlint(messagePath)

		Expect(err).NotTo(HaveOccurred())
		Expect(output).To(ContainSubstring("Commit message format looks good"))
	})

	It("rejects invalid commit messages", func() {
		messagePath := writeMessage("this is not conventional\n")

		output, err := runCommitlint(messagePath)

		Expect(err).To(HaveOccurred())
		var exitErr *exec.ExitError
		Expect(err).To(BeAssignableToTypeOf(exitErr))
		Expect(output).To(ContainSubstring("does not follow Conventional Commits format"))
	})

	It("returns an error when commit message file cannot be read", func() {
		missingPath := filepath.Join(GinkgoT().TempDir(), "missing-msg")

		output, err := runCommitlint(missingPath)

		Expect(err).To(HaveOccurred())
		var exitErr *exec.ExitError
		Expect(err).To(BeAssignableToTypeOf(exitErr))
		Expect(strings.ToLower(output)).To(ContainSubstring("error reading commit message"))
	})

	It("accepts valid messages without scope", func() {
		messagePath := writeMessage("docs: update README with usage examples\n")

		output, err := runCommitlint(messagePath)

		Expect(err).NotTo(HaveOccurred())
		Expect(output).To(ContainSubstring("Commit message format looks good"))
	})
})
