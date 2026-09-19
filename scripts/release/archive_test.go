package main

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVSIXAndArchivesUseTheSameServer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		binaries []string
		valid    bool
	}{
		{"matching", []string{"same server"}, true},
		{"mismatched", []string{"old server"}, false},
		{"missing", nil, false},
		{"duplicate", []string{"same server", "same server"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			server := filepath.Join(directory, "bingo")
			if err := os.WriteFile(server, []byte("same server"), 0o755); err != nil {
				t.Fatal(err)
			}
			vsix := filepath.Join(directory, "bingo.vsix")
			file, err := os.Create(vsix)
			if err != nil {
				t.Fatal(err)
			}
			archive := zip.NewWriter(file)
			for _, binary := range tc.binaries {
				writer, err := archive.Create("extension/bin/bingo")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write([]byte(binary)); err != nil {
					t.Fatal(err)
				}
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := verifyVSIXServer(vsix, server); (err == nil) != tc.valid {
				t.Fatalf("unexpected server identity verdict: %v", err)
			}
		})
	}
}

func TestArchiveContentsModesAndReproducibility(t *testing.T) {
	directory := t.TempDir()
	server := filepath.Join(directory, "server")
	if err := os.WriteFile(server, []byte("native server"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries := []archiveEntry{
		{name: "bin/bingo", source: server, mode: 0o755},
		{name: "INSTALL.txt", data: []byte("setup"), mode: 0o644},
		{name: "LICENSE", data: []byte("MIT"), mode: 0o644},
	}
	first := filepath.Join(directory, "first.tar.gz")
	second := filepath.Join(directory, "second.tar.gz")
	if err := writeArchive(first, "bingo_v0.7.0_darwin_arm64", entries); err != nil {
		t.Fatal(err)
	}
	if err := verifyArchive(first, "bingo_v0.7.0_darwin_arm64", entries); err != nil {
		t.Fatal(err)
	}
	entries[0], entries[2] = entries[2], entries[0]
	if err := writeArchive(second, "bingo_v0.7.0_darwin_arm64", entries); err != nil {
		t.Fatal(err)
	}
	firstHash, err := fileHash(first)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := fileHash(second)
	if err != nil || firstHash != secondHash {
		t.Fatalf("archive depends on input order or output filename: %s, %s, %v", firstHash, secondHash, err)
	}
	entries[2].mode = 0o644
	if err := verifyArchive(first, "bingo_v0.7.0_darwin_arm64", entries); err == nil {
		t.Fatal("accepted changed binary mode")
	}
	entries[2].mode = 0o755
	if err := os.WriteFile(server, []byte("wrong content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyArchive(first, "bingo_v0.7.0_darwin_arm64", entries); err == nil {
		t.Fatal("accepted changed binary content")
	}
}

func TestArchiveRejectsUnsafeOrDuplicateEntries(t *testing.T) {
	for _, name := range []string{"", ".", "..", "/absolute", "../escape", "a/../../escape", "a/../b", "a\\b", "a\nb", "a/"} {
		t.Run(name, func(t *testing.T) {
			if err := writeArchive(filepath.Join(t.TempDir(), "bad.tar.gz"), "bundle", []archiveEntry{
				{name: name, data: []byte("bad"), mode: 0o644},
			}); err == nil {
				t.Fatalf("accepted unsafe archive path %q", name)
			}
		})
	}
	if _, err := sortedEntries("../escape", nil); err == nil {
		t.Fatal("accepted unsafe archive root")
	}
	if _, err := sortedEntries("bundle", []archiveEntry{
		{name: "LICENSE", mode: 0o644}, {name: "LICENSE", mode: 0o644},
	}); err == nil {
		t.Fatal("accepted duplicate path")
	}
	if _, err := sortedEntries("bundle", []archiveEntry{{name: "bin/bingo", mode: 0o4755}}); err == nil {
		t.Fatal("accepted setuid archive file")
	}
}

func TestArchiveRejectsSymlinksAndMissingCompanion(t *testing.T) {
	root := t.TempDir()
	if _, err := directoryEntries(root, "lua"); err == nil {
		t.Fatal("accepted missing Lua companion")
	}
	if err := os.Mkdir(filepath.Join(root, "lua"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := directoryEntries(root, "lua"); err == nil {
		t.Fatal("accepted empty Lua companion")
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "lua/link.lua")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := directoryEntries(root, "lua"); err == nil {
		t.Fatal("accepted a symlink in the companion")
	}
	if _, err := (archiveEntry{source: link}).bytes(); err == nil {
		t.Fatal("followed an archive input symlink")
	}
}

func TestEndUserChecksumsAllowOnlyVerifiedSubsets(t *testing.T) {
	for _, state := range []string{"selected asset", "tampered selected asset", "no present assets"} {
		t.Run(state, func(t *testing.T) {
			checkEndUserChecksumSubset(t, state)
		})
	}
}

func checkEndUserChecksumSubset(t *testing.T, state string) {
	t.Helper()
	directory := t.TempDir()
	assets := []string{"bundle.tar.gz", "extension.vsix", "companion.tar.gz", "metadata.json"}
	const sums = "bingo_v0.7.0_darwin_arm64_SHA256SUMS.txt"
	writeChecksumFixture(t, directory, sums, assets)
	for _, name := range assets[1:] {
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	switch state {
	case "tampered selected asset":
		if err := os.WriteFile(filepath.Join(directory, assets[0]), []byte("tampered"), 0o644); err != nil {
			t.Fatal(err)
		}
	case "no present assets":
		if err := os.Remove(filepath.Join(directory, assets[0])); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("shasum", "-a", "256", "-c", "--ignore-missing", sums)
	cmd.Dir = directory
	out, err := cmd.CombinedOutput()
	if state == "selected asset" {
		if err != nil || strings.TrimSpace(string(out)) != assets[0]+": OK" {
			t.Fatalf("selected asset was not explicitly verified: %v\n%s", err, out)
		}
	} else if err == nil || strings.Contains(string(out), ": OK") {
		t.Fatalf("unverified subset reported success: %v\n%s", err, out)
	}
}

func writeChecksumFixture(t *testing.T, directory, sums string, assets []string) {
	t.Helper()
	for _, name := range assets {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeChecksums(directory, sums, assets); err != nil {
		t.Fatal(err)
	}
}

func TestChecksumsPinExactBasenamesAndContent(t *testing.T) {
	directory := t.TempDir()
	assets := []string{"bundle.tar.gz", "extension.vsix"}
	for _, name := range assets {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const sums = "bingo_v0.7.0_darwin_arm64_SHA256SUMS.txt"
	if err := writeChecksums(directory, sums, assets); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksums(directory, sums, assets); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, sums))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		hash, name, ok := strings.Cut(line, "  ")
		if !ok || len(hash) != 64 || filepath.Base(name) != name {
			t.Fatalf("ambiguous checksum line %q", line)
		}
	}
	if err := verifyChecksums(directory, sums, assets[:1]); err == nil {
		t.Fatal("accepted extra checksum entry")
	}
	if err := os.WriteFile(filepath.Join(directory, assets[0]), []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksums(directory, sums, assets); err == nil {
		t.Fatal("accepted corrupted asset")
	}
	for _, names := range [][]string{{"../escape"}, {"bundle.tar.gz", "bundle.tar.gz"}, {"dir/asset"}} {
		if _, err := checksumContent(directory, names); err == nil {
			t.Fatalf("accepted ambiguous checksum names %q", names)
		}
	}
}
