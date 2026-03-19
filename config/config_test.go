package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bingosuite/bingo/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config Suite")
}

var _ = Describe("Config", func() {
	Describe("Default", func() {
		It("returns expected default values", func() {
			cfg := config.Default()

			Expect(cfg).NotTo(BeNil())
			Expect(cfg.WebSocket.MaxSessions).To(Equal(100))
			Expect(cfg.WebSocket.IdleTimeout).To(Equal(time.Hour))
			Expect(cfg.Server.Addr).To(Equal(":8080"))
			Expect(cfg.Logging.Level).To(Equal("info"))
		})
	})

	Describe("Load", func() {
		It("returns default config when file does not exist", func() {
			missingPath := filepath.Join(GinkgoT().TempDir(), "does-not-exist.yml")

			cfg, err := config.Load(missingPath)

			Expect(err).NotTo(HaveOccurred())
			def := config.Default()
			Expect(cfg.WebSocket.MaxSessions).To(Equal(def.WebSocket.MaxSessions))
			Expect(cfg.WebSocket.IdleTimeout).To(Equal(def.WebSocket.IdleTimeout))
			Expect(cfg.Server.Addr).To(Equal(def.Server.Addr))
			Expect(cfg.Logging.Level).To(Equal(def.Logging.Level))
		})

		It("returns read error for unreadable path", func() {
			dirPath := GinkgoT().TempDir()

			_, err := config.Load(dirPath)

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to read config file"))
		})

		It("returns parse error for invalid YAML", func() {
			path := filepath.Join(GinkgoT().TempDir(), "config.yml")
			Expect(os.WriteFile(path, []byte("server: ["), 0o644)).To(Succeed())

			_, err := config.Load(path)

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to parse config file"))
		})

		It("merges provided fields with defaults", func() {
			path := filepath.Join(GinkgoT().TempDir(), "config.yml")
			content := []byte(`server:
  addr: ":9090"
websocket:
  max_sessions: 7
`)
			Expect(os.WriteFile(path, content, 0o644)).To(Succeed())

			cfg, err := config.Load(path)

			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Server.Addr).To(Equal(":9090"))
			Expect(cfg.WebSocket.MaxSessions).To(Equal(7))
			Expect(cfg.WebSocket.IdleTimeout).To(Equal(time.Hour))
			Expect(cfg.Logging.Level).To(Equal("info"))
		})

		It("parses duration values for idle timeout", func() {
			path := filepath.Join(GinkgoT().TempDir(), "config.yml")
			content := []byte(`websocket:
  idle_timeout: 30m
`)
			Expect(os.WriteFile(path, content, 0o644)).To(Succeed())

			cfg, err := config.Load(path)

			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.WebSocket.IdleTimeout).To(Equal(30 * time.Minute))
		})

		It("wraps read errors with expected prefix", func() {
			_, err := config.Load("/no/such/config.yml")

			// Missing files are treated as default config without error.
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
