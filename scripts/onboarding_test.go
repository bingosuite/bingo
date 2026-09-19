package tooling_test

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestOnboardingRelativeLinks(t *testing.T) {
	linkPattern := regexp.MustCompile(`\[[^\]]+\]\(([^ )]+)\)`)
	for _, name := range []string{"README.md", "docs/SETUP.md", "docs/ConcurrencyTelemetry.md"} {
		t.Run(name, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join("..", name))
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range linkPattern.FindAllStringSubmatch(string(content), -1) {
				link, err := url.Parse(match[1])
				if err != nil {
					t.Errorf("invalid link %q: %v", match[1], err)
					continue
				}
				if link.IsAbs() || link.Path == "" {
					continue
				}
				path := filepath.Join("..", filepath.Dir(name), filepath.FromSlash(link.Path))
				if _, err := os.Stat(path); err != nil {
					t.Errorf("broken relative link %q: %v", match[1], err)
				}
			}
		})
	}
}

func TestJustSeparatesStartupInstallAndRelease(t *testing.T) {
	content, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, want := range []string{
		"default:\n\t@just --list",
		"vscode-install:\n\tbash ./scripts/package-vscode.sh install",
		"vscode-local-package:\n\tbash ./scripts/package-vscode.sh",
		"vscode-package: vscode-check",
		"npm --prefix editors/vscode run package:reproducible",
		"neovim-prepare:\n\tbash ./editors/neovim/scripts/prepare.sh",
		"go run ./scripts/release",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing recipe contract %q", want)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "go clean") ||
			(strings.HasPrefix(line, "server") && strings.Contains(line, "build-target")) ||
			(strings.HasPrefix(line, "vscode-dev:") && strings.Contains(line, "build-examples")) {
			t.Errorf("startup still does unrelated build work: %s", line)
		}
	}
}
