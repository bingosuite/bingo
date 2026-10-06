package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCoreAssetsNeedNoEditorCheckout(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("MIT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bingo", "bingo-cli", "bingo-dapcli", "bingo-wsmon"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := metadata{Version: "v1.2.3", Commit: strings.Repeat("a", 40), GOOS: "linux", GOARCH: "amd64", WireProtocolVersion: "1.4"}
	assets, err := stageReleaseAssets(root, root, bin, m)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bingo_v1.2.3_linux_amd64.tar.gz", "bingo_v1.2.3_linux_amd64.json", "bingo_v1.2.3_linux_amd64_SHA256SUMS.txt"}
	if !reflect.DeepEqual(assets, want) {
		t.Fatalf("core artifact contract: got %v, want %v", assets, want)
	}
	if err := verifyChecksums(root, want[2], want[:2]); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, want[1]))
	if err != nil || strings.Contains(string(data), "vscodeVersion") {
		t.Fatalf("metadata still couples editor versions to core: %s, %v", data, err)
	}
}
