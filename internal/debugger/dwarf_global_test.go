package debugger

import (
	"debug/dwarf"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const scopedGlobalsModule = "example.com/scopedglobals"

var scopedGlobalsFiles = map[string]string{
	"go.mod": `module example.com/scopedglobals

go 1.25
`,
	"main.go": `package main

import (
	"example.com/scopedglobals/asmfixture"
	"example.com/scopedglobals/left"
	"example.com/scopedglobals/right"
)

var buildVersion = "v1.2.3"
var initValue string

func init() {
	initValue = buildVersion
}

//go:noinline
func frame() string {
	return buildVersion
}

type receiver struct{}

//go:noinline
func (receiver) method() string {
	return buildVersion
}

func makeClosure() func() string {
	return func() string {
		return buildVersion
	}
}

var closure = makeClosure()

func main() {
	println(frame(), receiver{}.method(), closure(), left.Frame(), right.Frame(), right.Fallback(), asmfixture.Read(), initValue)
}
`,
	"left/left.go": `package left

var BuildVersion = "left"

//go:noinline
func Frame() string {
	return BuildVersion
}
`,
	"right/right.go": `package right

var BuildVersion = "right"
var FallbackOnly = "fallback"

//go:noinline
func Frame() string {
	return BuildVersion
}

//go:noinline
func Fallback() string {
	return FallbackOnly
}
`,
	"asmfixture/asmfixture.go": `package asmfixture

var BuildVersion = "assembly"

func AssemblyFrame()

//go:noinline
func Read() string {
	AssemblyFrame()
	return BuildVersion
}
`,
	"asmfixture/frame.s": `#include "textflag.h"

TEXT ·AssemblyFrame(SB),NOSPLIT,$0-0
	RET
`,
}

func buildScopedGlobalsFixture(t *testing.T, gcflags string) *dwarfReader {
	t.Helper()
	root := t.TempDir()
	for name, contents := range scopedGlobalsFiles {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}

	binaryPath := filepath.Join(root, "fixture")
	args := []string{"build", "-o", binaryPath}
	if gcflags != "" {
		args = append(args, "-gcflags="+gcflags)
	}
	args = append(args, ".")
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build scoped-global fixture: %v\n%s", err, out)
	}

	reader, err := openDWARF(binaryPath)
	if err != nil {
		t.Fatalf("open fixture DWARF: %v", err)
	}
	reader.slide = 0x12345000
	return reader
}

func subprogramPC(t *testing.T, r *dwarfReader, suffix string) uint64 {
	t.Helper()
	rd := r.data.Reader()
	var names []string
	for {
		entry, err := rd.Next()
		if err != nil {
			t.Fatalf("scan subprograms: %v", err)
		}
		if entry == nil {
			break
		}
		if entry.Tag != dwarf.TagSubprogram {
			continue
		}
		name, _ := entry.Val(dwarf.AttrName).(string)
		if name != "" && len(names) < 100 &&
			(strings.Contains(name, "main") || strings.Contains(name, scopedGlobalsModule)) {
			names = append(names, name)
		}
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		lowPC, ok := entry.Val(dwarf.AttrLowpc).(uint64)
		if ok {
			return uint64(int64(lowPC) + r.slide)
		}
	}
	t.Fatalf("no subprogram ending in %q; names include %v", suffix, names)
	return 0
}

func exactGlobalAddress(t *testing.T, r *dwarfReader, name string) uint64 {
	t.Helper()
	addr := r.resolveVarAddr(name)
	if addr == 0 {
		t.Fatalf("no exact global %q", name)
	}
	return addr
}

func assertEvaluatesGlobal(t *testing.T, r *dwarfReader, pc uint64, query, exact string) {
	t.Helper()
	got, err := r.EvaluateName(readableBackend{}, pc, 0, query)
	if err != nil {
		t.Fatalf("EvaluateName(%#x, %q): %v", pc, query, err)
	}
	want := exactGlobalAddress(t, r, exact)
	if got.Address != want {
		t.Fatalf("EvaluateName(%#x, %q) address = %#x, want %s at %#x",
			pc, query, got.Address, exact, want)
	}
}

func TestEvaluateNamePrefersFramePackageGlobals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gcflags string
	}{
		{name: "optimized"},
		{name: "no_optimize_no_inline", gcflags: "all=-N -l"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildScopedGlobalsFixture(t, tc.gcflags)

			mainPC := subprogramPC(t, r, "main.frame")
			leftPC := subprogramPC(t, r, "/left.Frame")
			rightPC := subprogramPC(t, r, "/right.Frame")
			methodPC := subprogramPC(t, r, "main.receiver.method")
			closurePC := subprogramPC(t, r, "makeClosure.func1")
			initPC := subprogramPC(t, r, "main.init.0")
			assemblyPC := subprogramPC(t, r, "/asmfixture.AssemblyFrame")

			t.Run("runtime collision", func(t *testing.T) {
				assertEvaluatesGlobal(t, r, mainPC, "buildVersion", "main.buildVersion")
			})
			t.Run("same name in two packages", func(t *testing.T) {
				assertEvaluatesGlobal(t, r, leftPC, "BuildVersion", scopedGlobalsModule+"/left.BuildVersion")
				assertEvaluatesGlobal(t, r, rightPC, "BuildVersion", scopedGlobalsModule+"/right.BuildVersion")
			})
			t.Run("method closure and init frames", func(t *testing.T) {
				assertEvaluatesGlobal(t, r, methodPC, "buildVersion", "main.buildVersion")
				assertEvaluatesGlobal(t, r, closurePC, "buildVersion", "main.buildVersion")
				assertEvaluatesGlobal(t, r, initPC, "buildVersion", "main.buildVersion")
			})
			t.Run("assembly CU shares package globals", func(t *testing.T) {
				assertEvaluatesGlobal(t, r, assemblyPC, "BuildVersion", scopedGlobalsModule+"/asmfixture.BuildVersion")
			})
			t.Run("cross package fallback", func(t *testing.T) {
				assertEvaluatesGlobal(t, r, leftPC, "FallbackOnly", scopedGlobalsModule+"/right.FallbackOnly")
				assertEvaluatesGlobal(t, r, ^uint64(0), "FallbackOnly", scopedGlobalsModule+"/right.FallbackOnly")
			})
			t.Run("qualified names stay exact", func(t *testing.T) {
				assertEvaluatesGlobal(t, r, mainPC, "main.buildVersion", "main.buildVersion")
				assertEvaluatesGlobal(t, r, mainPC, scopedGlobalsModule+"/left.BuildVersion", scopedGlobalsModule+"/left.BuildVersion")
				assertEvaluatesGlobal(t, r, mainPC, "runtime.buildVersion", "runtime.buildVersion")
			})
		})
	}
}

func TestPackageForPCUsesRuntimeSlide(t *testing.T) {
	r := buildScopedGlobalsFixture(t, "")
	pc := subprogramPC(t, r, "/right.Frame")

	got, ok := r.packageForPC(pc)
	if !ok {
		t.Fatal("packageForPC did not resolve a slid runtime PC")
	}
	if want := scopedGlobalsModule + "/right"; got != want {
		t.Fatalf("packageForPC(%#x) = %q, want %q", pc, got, want)
	}
	if pc < uint64(r.slide) {
		t.Fatalf("test fixture PC %#x does not include slide %#x", pc, r.slide)
	}
}
