package dap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sourceFixture(t *testing.T, source string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "package with spaces")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"go.mod":  "module example.test/source\n\ngo 1.25.0\n",
		"main.go": source,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func requireEmptyBuildRoot(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned build root not retired: %v, %v", entries, err)
	}
}

func TestSourceBuilderRealPackage(t *testing.T) {
	dir := sourceFixture(t, `package main
import ("fmt"; "os")
func main() {
	data, err := os.ReadFile("data.txt")
	if err != nil { panic(err) }
	fmt.Printf("%s|%s|%s", data, os.Getenv("PWD"), os.Args[1])
}
`)
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("package data"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	b := sourceBuilder{tempDir: root}
	ctx, cancel := context.WithTimeout(context.Background(), sourceBuildTimeout)
	defer cancel()
	artifact, err := b.build(ctx, launchConfig{Program: dir, Env: []string{"GOWORK=off", "GOTOOLCHAIN=local"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := artifact.remove(); err != nil {
			t.Error(err)
		}
	}()
	cmd := exec.Command(artifact.program, "argument with spaces")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "package data|"+dir+"|argument with spaces" {
		t.Fatalf("built child = %q, %v", output, err)
	}
}

func TestSourceBuilderCommandAndDiagnostics(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	wantFailure := errors.New("compiler failed")
	b := sourceBuilder{tempDir: root, run: func(cmd *exec.Cmd) error {
		want := []string{"build", "-gcflags=all=-N -l", "-o", cmd.Args[4], "."}
		if !reflect.DeepEqual(cmd.Args[1:], want) || cmd.Dir != dir ||
			filepath.Dir(filepath.Dir(cmd.Args[4])) != root {
			t.Fatalf("build command = %v, dir %q", cmd.Args, cmd.Dir)
		}
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.Cancel == nil || cmd.WaitDelay <= 0 {
			t.Fatal("compiler cancellation must own a process group and bounded pipe join")
		}
		if !strings.Contains(strings.Join(cmd.Env, "\n"), "BINGO_BUILD_VALUE=override\n") {
			t.Fatalf("missing build environment override")
		}
		for _, name := range []string{"GOTMPDIR", "TMPDIR"} {
			if !strings.Contains(strings.Join(cmd.Env, "\n"), name+"="+filepath.Dir(cmd.Args[4])+"\n") {
				t.Fatalf("%s must keep compiler scratch inside the owned build directory", name)
			}
		}
		if _, err := cmd.Stdout.Write([]byte(strings.Repeat("x", buildOutputLimit*4))); err != nil {
			t.Fatal(err)
		}
		return wantFailure
	}}
	artifact, err := b.build(context.Background(), launchConfig{Program: dir, Env: []string{"BINGO_BUILD_VALUE=override"}})
	if artifact != nil || !errors.Is(err, wantFailure) || !strings.Contains(err.Error(), buildOutputCut) {
		t.Fatalf("failed build = %+v, %v", artifact, err)
	}
	if len(err.Error()) > buildOutputLimit+100 {
		t.Fatalf("build diagnostics are unbounded: %d bytes", len(err.Error()))
	}
	requireEmptyBuildRoot(t, root)
}

func TestSourceBuilderRejectsLateSuccess(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	b := sourceBuilder{tempDir: root, run: func(cmd *exec.Cmd) error {
		<-ctx.Done()
		return os.WriteFile(cmd.Args[4], []byte("late output"), 0o700)
	}}
	artifact, err := b.build(ctx, launchConfig{Program: t.TempDir()})
	if artifact != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late success = %+v, %v", artifact, err)
	}
	requireEmptyBuildRoot(t, root)
}

func TestSourceBuilderMissingGo(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	artifact, err := (sourceBuilder{tempDir: root}).build(context.Background(), launchConfig{})
	if artifact != nil || err == nil || !strings.Contains(err.Error(), "server PATH") {
		t.Fatalf("missing go = %+v, %v", artifact, err)
	}
	requireEmptyBuildRoot(t, root)
}

func TestSourceBuilderCompileErrorAndNonMain(t *testing.T) {
	for _, source := range []string{"package main\nfunc main() { undefined() }\n", "package library\nfunc Exported() {}\n"} {
		dir := sourceFixture(t, source)
		root := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), sourceBuildTimeout)
		artifact, err := (sourceBuilder{tempDir: root}).build(ctx, launchConfig{
			Program: dir, Env: []string{"GOWORK=off", "GOTOOLCHAIN=local"},
		})
		cancel()
		if artifact != nil || err == nil {
			t.Fatalf("invalid source succeeded: %+v, %v", artifact, err)
		}
		if !strings.Contains(err.Error(), "undefined") && !strings.Contains(err.Error(), "main package") {
			t.Fatalf("build cause missing: %v", err)
		}
		requireEmptyBuildRoot(t, root)
	}
}

func TestSourceBuilderCancellationKillsCompilerGroup(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "compiler.pid")
	started := make(chan int, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := sourceBuilder{tempDir: root, run: func(cmd *exec.Cmd) error {
		cmd.Path = executable
		cmd.Args = []string{executable, "-test.run=^TestSourceCompilerProcess$"}
		cmd.Env = append(cmd.Env, "BINGO_COMPILER_HELPER=parent", "BINGO_COMPILER_PID="+pidFile)
		if err := cmd.Start(); err != nil {
			return err
		}
		started <- cmd.Process.Pid
		return cmd.Wait()
	}}
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, err := b.build(ctx, launchConfig{Program: root})
		result <- err
	}()
	defer func() {
		cancel()
		awaitSourceSignal(t, finished)
	}()
	var parentPID int
	select {
	case parentPID = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("compiler helper did not start")
	}
	deadline := time.Now().Add(5 * time.Second)
	var childPID int
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, err = strconv.Atoi(string(data))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("compiler descendant did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled compiler error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled compiler was not joined")
	}
	requireEmptyBuildRoot(t, root)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(-parentPID, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owned compiler process group %d still contains descendant %d", parentPID, childPID)
}

func TestSourceCompilerProcess(t *testing.T) {
	switch os.Getenv("BINGO_COMPILER_HELPER") {
	case "parent":
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestSourceCompilerProcess$")
		cmd.Env = append(os.Environ(), "BINGO_COMPILER_HELPER=child")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	case "child":
		if err := os.WriteFile(os.Getenv("BINGO_COMPILER_PID"), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
}
