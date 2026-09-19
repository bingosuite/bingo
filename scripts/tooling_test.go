package tooling_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildBinary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    []string
		target []string
		want   []string
		absent []string
	}{
		{
			name: "linux",
			want: []string{"cgo=0 target=linux/amd64", "-trimpath", "-buildvcs=false", "main.buildVersion=v0.7.0", "main.buildCommit=" + strings.Repeat("a", 40)},
			absent: []string{
				"codesign", "normalize", "bingonative",
			},
		},
		{
			name: "darwin without Node",
			env:  []string{"FAKE_OS=Darwin", "FAKE_ARCH=arm64", "FAKE_NODE_FAIL=1"},
			want: []string{"cgo=1 target=darwin/arm64", "-tags bingonative", "--identifier bingosuite.bingo --entitlements", "codesign --verify --strict"},
			absent: []string{
				"normalize",
			},
		},
		{
			name: "reproducible darwin",
			env:  []string{"FAKE_OS=Darwin", "FAKE_ARCH=arm64", "BINGO_REPRODUCIBLE=1"},
			want: []string{"normalize", "codesign --verify --strict"},
		},
		{
			name:   "explicit linux cross compile",
			env:    []string{"FAKE_OS=Darwin", "FAKE_ARCH=arm64"},
			target: []string{"linux", "amd64"},
			want:   []string{"cgo=0 target=linux/amd64"},
			absent: []string{"codesign", "normalize"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newToolingFixture(t)
			args := append([]string{"bingo", f.output}, tc.target...)
			out, err := f.run(tc.env, args...)
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			log := f.log()
			for _, want := range tc.want {
				if !strings.Contains(log, want) {
					t.Fatalf("missing %q in commands:\n%s", want, log)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(log, absent) {
					t.Fatalf("unexpected %q in commands:\n%s", absent, log)
				}
			}
			if strings.Contains(log, "normalize") && strings.Index(log, "normalize") > strings.Index(log, "codesign --sign") {
				t.Fatalf("normalized after signing:\n%s", log)
			}
			info, err := os.Stat(f.output)
			if err != nil || info.Mode().Perm() != 0o755 {
				t.Fatalf("output is not executable: %v, %v", info, err)
			}
			f.noTemporaryOutput()
		})
	}
}

func TestBuildBinaryFailuresPreserveInstalledBinary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    []string
		args   []string
		want   string
		noWork bool
	}{
		{"unsupported host", []string{"FAKE_ARCH=aarch64"}, nil, "unsupported host", true},
		{"unsupported target", nil, []string{"linux", "arm64"}, "unsupported build target", true},
		{"Darwin cross compile", nil, []string{"darwin", "arm64"}, "require native Apple Silicon", true},
		{"old Go", []string{"FAKE_GO_VERSION=go1.25.4"}, nil, "Go 1.25.5 or newer", true},
		{"Go unavailable", []string{"FAKE_GO_FAIL=1"}, nil, "cannot run the Go toolchain", true},
		{"invalid version", []string{"BINGO_VERSION=v0.7.0;touch unsafe"}, nil, "version must be", true},
		{"invalid commit", []string{"BINGO_COMMIT=HEAD"}, nil, "commit must be", true},
		{"invalid reproducibility", []string{"BINGO_REPRODUCIBLE=yes"}, nil, "must be 0 or 1", true},
		{"old Node", []string{"FAKE_OS=Darwin", "FAKE_ARCH=arm64", "BINGO_REPRODUCIBLE=1", "FAKE_NODE_VERSION=v20.0.0"}, nil, "Node.js 22.x", true},
		{"build failure", []string{"FAKE_BUILD_FAIL=1"}, nil, "injected build failure", false},
		{"sign failure", []string{"FAKE_OS=Darwin", "FAKE_ARCH=arm64", "FAKE_SIGN_FAIL=1"}, nil, "injected sign failure", false},
		{"verify failure", []string{"FAKE_OS=Darwin", "FAKE_ARCH=arm64", "FAKE_VERIFY_FAIL=1"}, nil, "injected verify failure", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newToolingFixture(t)
			if err := os.WriteFile(f.output, []byte("previous binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"bingo", f.output}, tc.args...)
			out, err := f.run(tc.env, args...)
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("wanted failure %q, got %v\n%s", tc.want, err, out)
			}
			got, err := os.ReadFile(f.output)
			if err != nil || string(got) != "previous binary" {
				t.Fatalf("replaced prior binary after failure: %q, %v", got, err)
			}
			if tc.noWork && f.log() != "" {
				t.Fatalf("preflight failure performed expensive work:\n%s", f.log())
			}
			f.noTemporaryOutput()
		})
	}
}

type toolingFixture struct {
	t      *testing.T
	root   string
	bin    string
	output string
}

func newToolingFixture(t *testing.T) toolingFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "checkout with spaces")
	f := toolingFixture{t: t, root: root, bin: filepath.Join(root, "fake-bin"), output: filepath.Join(root, "installed bingo")}
	for _, dir := range []string{"scripts", "fake-bin", "editors/vscode"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"build-binary.sh", "tooling.sh"} {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f.write(filepath.Join("scripts", name), string(content))
	}
	f.write("go.mod", "module test.invalid/tooling\n\ngo 1.25.5\n")
	f.write("editors/vscode/.nvmrc", "22\n")
	f.write("fake-bin/uname", `#!/bin/bash
case "$1" in
  -s) echo "${FAKE_OS:-Linux}" ;;
  -m) echo "${FAKE_ARCH:-x86_64}" ;;
  *) exit 2 ;;
esac
`)
	f.write("fake-bin/go", `#!/bin/bash
if [[ "$1" == env ]]; then
  [[ "${FAKE_GO_FAIL:-0}" == 0 ]] || exit 1
  echo "${FAKE_GO_VERSION:-go1.25.5}"
  exit
fi
echo "go cgo=$CGO_ENABLED target=$GOOS/$GOARCH $*" >> "$BINGO_TEST_LOG"
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == -o ]]; then shift; printf 'new binary\n' > "$1"; break; fi
  shift
done
if [[ "${FAKE_BUILD_FAIL:-0}" == 1 ]]; then echo 'injected build failure' >&2; exit 1; fi
`)
	f.write("fake-bin/node", `#!/bin/bash
[[ "${FAKE_NODE_FAIL:-0}" == 0 ]] || { echo 'Node should not be needed' >&2; exit 1; }
if [[ "$1" == --version ]]; then echo "${FAKE_NODE_VERSION:-v22.17.0}"; exit; fi
echo normalize >> "$BINGO_TEST_LOG"
`)
	f.write("fake-bin/xcrun", "#!/bin/bash\necho /test/clang\n")
	f.write("fake-bin/codesign", `#!/bin/bash
echo "codesign $*" >> "$BINGO_TEST_LOG"
if [[ "$1" == --sign && "${FAKE_SIGN_FAIL:-0}" == 1 ]]; then echo 'injected sign failure' >&2; exit 1; fi
if [[ "$1" == --verify && "${FAKE_VERIFY_FAIL:-0}" == 1 ]]; then echo 'injected verify failure' >&2; exit 1; fi
exit 0
`)
	return f
}

func (f toolingFixture) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name), []byte(content), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f toolingFixture) run(env []string, args ...string) ([]byte, error) {
	f.t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(f.root, "scripts/build-binary.sh")}, args...)...)
	cmd.Dir = f.t.TempDir()
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "BINGO_") && !strings.HasPrefix(item, "FAKE_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env,
		"PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BINGO_TEST_LOG="+filepath.Join(f.root, "commands.log"),
		"BINGO_VERSION=v0.7.0",
		"BINGO_COMMIT="+strings.Repeat("a", 40),
	)
	cmd.Env = append(cmd.Env, env...)
	return cmd.CombinedOutput()
}

func (f toolingFixture) log() string {
	f.t.Helper()
	content, err := os.ReadFile(filepath.Join(f.root, "commands.log"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return string(content)
}

func (f toolingFixture) noTemporaryOutput() {
	f.t.Helper()
	matches, err := filepath.Glob(f.output + ".*")
	if err != nil || len(matches) != 0 {
		f.t.Fatalf("temporary output was not cleaned: %v, %v", matches, err)
	}
}
