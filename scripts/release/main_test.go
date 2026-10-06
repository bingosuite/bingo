package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseTagValidationAgreesWithShell(t *testing.T) {
	for _, version := range []string{
		"v0.7.0", "v1.2.3-rc.1", "v1.2.3-preview-2", "v10.20.30",
		"dev", "", "-version", "1.2.3", "v01.2.3", "v1.2.3\n", "v1.2.3/branch",
		"v1.2.3;touch unsafe", "v1.2.3$(touch unsafe)", "v1.2.3+" + strings.Repeat("a", 81),
		"v1.2.3-" + strings.Repeat("a", 81),
	} {
		t.Run(version, func(t *testing.T) {
			cmd := exec.Command("bash", "../release-ref.sh", version)
			out, err := cmd.CombinedOutput()
			valid := len(version) <= 80 && versionPattern.MatchString(version)
			if valid != (err == nil) {
				t.Fatalf("Go/shell tag validation differs for %q: %v\n%s", version, err, out)
			}
			if valid && string(out) != "tag="+version+"\n" {
				t.Fatalf("unexpected output shape %q", out)
			}
		})
	}
}

func TestReleasePinsCleanTagAndCommit(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		value, err := capture(root, nil, "git", args...)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	git("init", "-q")
	manifest := filepath.Join(root, "README.md")
	if err := os.WriteFile(manifest, []byte(`{"version":"0.7.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "user.name=Tooling Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
	git("tag", "v0.7.0")
	head := git("rev-parse", "HEAD")
	m, err := sourceMetadata(root, "v0.7.0", head)
	if err != nil || m.Commit != head || m.Version != "v0.7.0" {
		t.Fatalf("clean tag metadata: %+v, %v", m, err)
	}
	if _, err := sourceMetadata(root, "v0.7.0", strings.Repeat("a", 40)); err == nil {
		t.Fatal("accepted a checkout different from the resolved release commit")
	}
	if _, err := sourceMetadata(root, "v0.7.1", ""); err == nil {
		t.Fatal("accepted a missing tag")
	}
	if err := os.WriteFile(manifest, []byte(`{"version":"0.8.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceMetadata(root, "v0.7.0", ""); err == nil {
		t.Fatal("accepted a dirty release checkout")
	}
	dev, err := sourceMetadata(root, "dev", "")
	if err != nil || dev.Commit != head+"-dirty" {
		t.Fatalf("local preview did not name dirty source: %+v, %v", dev, err)
	}
	git("add", ".")
	git("-c", "user.name=Tooling Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "next")
	if _, err := sourceMetadata(root, "v0.7.0", ""); err == nil {
		t.Fatal("accepted a release tag that does not point at HEAD")
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.go"), []byte("package extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("tag", "v0.8.0")
	if _, err := sourceMetadata(root, "v0.8.0", ""); err == nil {
		t.Fatal("accepted untracked build inputs")
	}
}

func TestNativeReleaseTargets(t *testing.T) {
	for _, pair := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}} {
		if _, err := nativeTarget(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{{"linux", "arm64"}, {"darwin", "amd64"}, {"windows", "amd64"}} {
		if _, err := nativeTarget(pair[0], pair[1]); err == nil {
			t.Fatalf("accepted unsupported target %v", pair)
		}
	}
}

func TestBundledInstallInstructions(t *testing.T) {
	m := metadata{Version: "v0.7.0", Commit: strings.Repeat("a", 40), GOOS: "darwin", GOARCH: "arm64", WireProtocolVersion: "1.4"}
	text := installText(m)
	for _, want := range []string{
		"notarization", "do not require Go", "server's PATH", m.Commit + "/docs/SETUP.md",
		"SHA256SUMS", "--ignore-missing", "exact filename\nis reported OK", "No verified files",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("bundled onboarding is missing %q", want)
		}
	}
}
