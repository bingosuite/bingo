package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/bingosuite/bingo/pkg/protocol"
)

var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type metadata struct {
	Version             string `json:"version"`
	Commit              string `json:"commit"`
	GOOS                string `json:"goos"`
	GOARCH              string `json:"goarch"`
	GoVersion           string `json:"goVersion"`
	WireProtocolVersion string `json:"wireProtocolVersion"`
	VSCodeVersion       string `json:"vscodeVersion"`
	Signing             string `json:"signing"`
}

func main() {
	version := flag.String("version", "dev", "dev or the existing release tag checked out at HEAD")
	output := flag.String("output", "dist/release", "directory for verified native assets")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "release: unexpected positional arguments")
		os.Exit(2)
	}
	root, err := os.Getwd()
	if err == nil {
		err = packageRelease(root, *output, *version)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func packageRelease(root, output, version string) (retErr error) {
	m, err := sourceMetadata(root, version, os.Getenv("BINGO_COMMIT"))
	if err != nil {
		return err
	}
	target, err := nativeTarget(m.GOOS, m.GOARCH)
	if err != nil {
		return err
	}
	if err := run(root, nil, "bash", "scripts/preflight.sh", "release"); err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(output, ".build-"+target+"-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(scratch)) }()

	env := []string{
		"BINGO_VERSION=" + m.Version,
		"BINGO_COMMIT=" + m.Commit,
		"BINGO_VSCODE_TARGET=" + target,
		"BINGO_REPRODUCIBLE=1",
	}
	if err := run(root, env, "npm", "--prefix", "editors/vscode", "run", "package:reproducible"); err != nil {
		return err
	}
	if err := run(root, env, "npm", "--prefix", "editors/vscode", "run", "package:verify"); err != nil {
		return err
	}

	binaries := filepath.Join(scratch, "bin")
	if err := os.Mkdir(binaries, 0o755); err != nil {
		return err
	}
	server := filepath.Join(binaries, "bingo")
	if err := copyFile(filepath.Join(root, "editors/vscode/bin/bingo"), server, 0o755); err != nil {
		return err
	}
	versionOutput, err := capture(root, nil, server, "-version")
	if err != nil {
		return err
	}
	expectedVersion := fmt.Sprintf("bingo %s (commit %s; %s/%s; %s; wire %s)",
		m.Version, m.Commit, m.GOOS, m.GOARCH, m.GoVersion, m.WireProtocolVersion)
	if versionOutput != expectedVersion {
		return fmt.Errorf("packaged server has the wrong build identity: %s", versionOutput)
	}
	if err := verifyVSIXServer(filepath.Join(root, "dist/bingo-"+target+".vsix"), server); err != nil {
		return err
	}
	for _, command := range []string{"cli", "dapcli", "wsmon"} {
		binary := filepath.Join(binaries, "bingo-"+command)
		args := []string{"scripts/build-binary.sh", command, binary, m.GOOS, m.GOARCH}
		if err := run(root, env, "bash", args...); err != nil {
			return err
		}
		first, err := fileHash(binary)
		if err != nil {
			return err
		}
		if err := run(root, env, "bash", args...); err != nil {
			return err
		}
		second, err := fileHash(binary)
		if err != nil {
			return err
		}
		if first != second {
			return fmt.Errorf("%s is not reproducible: %s != %s", command, first, second)
		}
		if _, err := capture(root, nil, binary, "-help"); err != nil {
			return err
		}
	}

	base := "bingo_" + m.Version + "_" + m.GOOS + "_" + m.GOARCH
	neovimBase := "bingo-neovim_" + m.Version + "_" + m.GOOS + "_" + m.GOARCH
	vsixName := "bingo-" + m.Version + "-" + target + ".vsix"
	metadataBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	metadataBytes = append(metadataBytes, '\n')
	common := []archiveEntry{
		{name: "LICENSE", source: filepath.Join(root, "LICENSE"), mode: 0o644},
		{name: "RELEASE.json", data: metadataBytes, mode: 0o644},
	}
	terminal := append([]archiveEntry{}, common...)
	terminal = append(terminal, archiveEntry{name: "INSTALL.txt", data: []byte(installText(m, false)), mode: 0o644})
	for _, name := range []string{"bingo", "bingo-cli", "bingo-dapcli", "bingo-wsmon"} {
		terminal = append(terminal, archiveEntry{name: "bin/" + name, source: filepath.Join(binaries, name), mode: 0o755})
	}
	neovim := append([]archiveEntry{}, common...)
	neovim = append(neovim,
		archiveEntry{name: "INSTALL.txt", data: []byte(installText(m, true)), mode: 0o644},
		archiveEntry{name: "README.md", source: filepath.Join(root, "editors/neovim/README.md"), mode: 0o644},
		archiveEntry{name: "bin/bingo", source: server, mode: 0o755},
	)
	for _, dir := range []string{"lua", "plugin"} {
		entries, err := directoryEntries(filepath.Join(root, "editors/neovim"), dir)
		if err != nil {
			return err
		}
		neovim = append(neovim, entries...)
	}

	for _, archive := range []struct {
		name    string
		entries []archiveEntry
	}{
		{base, terminal},
		{neovimBase, neovim},
	} {
		path := filepath.Join(scratch, archive.name+".tar.gz")
		if err := writeArchive(path, archive.name, archive.entries); err != nil {
			return err
		}
		if err := verifyArchive(path, archive.name, archive.entries); err != nil {
			return err
		}
	}
	if err := copyFile(filepath.Join(root, "dist/bingo-"+target+".vsix"), filepath.Join(scratch, vsixName), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(scratch, base+".json"), metadataBytes, 0o644); err != nil {
		return err
	}
	assets := []string{base + ".tar.gz", neovimBase + ".tar.gz", vsixName, base + ".json"}
	checksums := base + "_SHA256SUMS.txt"
	if err := writeChecksums(scratch, checksums, assets); err != nil {
		return err
	}
	if err := verifyChecksums(scratch, checksums, assets); err != nil {
		return err
	}
	current, err := sourceMetadata(root, version, os.Getenv("BINGO_COMMIT"))
	if err != nil {
		return err
	}
	if current != m {
		return fmt.Errorf("source identity changed during packaging")
	}
	for _, name := range append(assets, checksums) {
		if err := os.Rename(filepath.Join(scratch, name), filepath.Join(output, name)); err != nil {
			return err
		}
		fmt.Println(filepath.Join(output, name))
	}
	return nil
}

func sourceMetadata(root, version, expectedCommit string) (metadata, error) {
	if len(version) > 80 || (version != "dev" && !versionPattern.MatchString(version)) {
		return metadata{}, fmt.Errorf("version must be dev or vMAJOR.MINOR.PATCH with an optional prerelease suffix (at most 80 characters)")
	}
	commit, err := capture(root, nil, "git", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return metadata{}, err
	}
	if !commitPattern.MatchString(commit) {
		return metadata{}, fmt.Errorf("HEAD is not a full Git commit SHA")
	}
	if expectedCommit != "" && expectedCommit != commit {
		return metadata{}, fmt.Errorf("HEAD does not match the resolved release commit")
	}
	status, err := capture(root, nil, "git", "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return metadata{}, err
	}
	if version != "dev" {
		if status != "" {
			return metadata{}, fmt.Errorf("release builds require a clean checkout; use -version dev for a local preview")
		}
		tagCommit, err := capture(root, nil, "git", "rev-parse", "--verify", "refs/tags/"+version+"^{commit}")
		if err != nil {
			return metadata{}, fmt.Errorf("resolve release tag: %w", err)
		}
		if tagCommit != commit {
			return metadata{}, fmt.Errorf("release tag does not point to HEAD")
		}
	} else if status != "" {
		commit += "-dirty"
	}
	var manifest struct {
		Version string `json:"version"`
	}
	data, err := os.ReadFile(filepath.Join(root, "editors/vscode/package.json"))
	if err != nil {
		return metadata{}, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return metadata{}, err
	}
	if manifest.Version == "" {
		return metadata{}, fmt.Errorf("VS Code manifest has no version")
	}
	signing := "none"
	if runtime.GOOS == "darwin" {
		signing = "ad-hoc; server has debugger entitlement; not notarized"
	}
	return metadata{
		Version: version, Commit: commit, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		GoVersion: runtime.Version(), WireProtocolVersion: protocol.Version,
		VSCodeVersion: manifest.Version, Signing: signing,
	}, nil
}

func nativeTarget(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "linux-x64", nil
	case "darwin/arm64":
		return "darwin-arm64", nil
	default:
		return "", fmt.Errorf("unsupported release host %s/%s", goos, goarch)
	}
}

func installText(m metadata, neovim bool) string {
	revision := strings.TrimSuffix(m.Commit, "-dirty")
	preamble := fmt.Sprintf("bingo %s (%s/%s)\nCommit: %s\nWire protocol: %s\n\n", m.Version, m.GOOS, m.GOARCH, m.Commit, m.WireProtocolVersion)
	usage := `The bin/ directory contains:
  bingo         Debug server (management/WebSocket and optional DAP).
  bingo-cli     Interactive WebSocket driver.
  bingo-dapcli  Interactive DAP driver.
  bingo-wsmon   Read-only concurrency telemetry observer.

Keep the extracted directory together or copy bin/* to a directory on PATH.
Check the build with ./bin/bingo -version. Start a manual server with:
  ./bin/bingo -addr 127.0.0.1:6060 -dap-addr 127.0.0.1:4711
In another terminal, run ./bin/bingo-dapcli (or ./bin/bingo-cli).
Use ./bin/bingo-wsmon -session SESSION_ID to observe an existing session.
Manual servers persist until stopped. Editors normally manage their own server.
`
	if neovim {
		usage = `This is a ready-to-use Neovim runtime directory, including bin/bingo.
Add the extracted directory to runtimepath or use it as a local plugin directory.
Install nvim-dap separately; require("bingo").setup() then :BingoDebug [directory].
No prepare hook or build is needed for this prebuilt bundle. Keep bin/ with lua/.
The companion never kills a potentially shared server.
`
	}
	return preamble + usage + `
Prebuilt debugger/client binaries do not require Go. Debugging a source package
does require a Go toolchain on the server's PATH; launching a prebuilt target does
not. Only native linux/amd64 and Apple Silicon darwin/arm64 are supported.
On macOS the server has an ad-hoc debugger-entitled signature, NOT notarization.
Do not disable Gatekeeper or SIP globally to install a downloaded binary.

Verify the separately downloaded platform SHA256SUMS file before installation:
  shasum -a 256 -c bingo_` + m.Version + `_` + m.GOOS + `_` + m.GOARCH + `_SHA256SUMS.txt
The checksum names are relative to the directory containing the downloaded assets.

Full setup and limitations:
https://github.com/bingosuite/bingo/blob/` + revision + `/docs/SETUP.md
Neovim usage:
https://github.com/bingosuite/bingo/blob/` + revision + `/editors/neovim/README.md
`
}

func run(root string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func capture(root string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
