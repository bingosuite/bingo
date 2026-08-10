//go:build linux && amd64 && e2e

package debugger

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

const nativeSentinelTarget = `
#define _GNU_SOURCE
#include <stdint.h>
#include <stdatomic.h>
#include <pthread.h>
#include <sched.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>
#include <sys/mman.h>
#include <unistd.h>

static const char *before_file;
static const char *unmapped_file;
static const char *release_file;
static const char *mapped_file;
static volatile unsigned int probe_value;

static void marker(const char *path) {
	int fd = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0600);
	if (fd >= 0) close(fd);
}

__attribute__((noinline, aligned(4096), section(".sentinel")))
static void sentinel_target(void) {
	probe_value = 1; // PROBE_BREAKPOINT
	probe_value += 1; // PROBE_SENTINEL
	probe_value += 2;
}

static void *remap_sentinel_page(void *unused) {
	(void)unused;
	long page_size = sysconf(_SC_PAGESIZE);
	uintptr_t page = (uintptr_t)&sentinel_target & ~((uintptr_t)page_size - 1);

	while (access(before_file, F_OK) != 0) sched_yield();

	unsigned char *saved = malloc((size_t)page_size);
	if (saved == NULL) _exit(70);
	memcpy(saved, (const void *)page, (size_t)page_size);
	if (munmap((void *)page, (size_t)page_size) != 0) _exit(71);
	marker(unmapped_file);

	while (access(release_file, F_OK) != 0) sched_yield();

	void *restored = mmap((void *)page, (size_t)page_size,
		PROT_READ | PROT_WRITE | PROT_EXEC,
		MAP_PRIVATE | MAP_ANONYMOUS | MAP_FIXED, -1, 0);
	if (restored == MAP_FAILED) _exit(72);
	memcpy(restored, saved, (size_t)page_size);
	__builtin___clear_cache((char *)restored, (char *)restored + page_size);
	if (mprotect(restored, (size_t)page_size, PROT_READ | PROT_EXEC) != 0) _exit(73);
	marker(mapped_file);
	return NULL;
}

int main(int argc, char **argv) {
	pthread_t remapper;
	if (argc == 5) {
		before_file = argv[1];
		unmapped_file = argv[2];
		release_file = argv[3];
		mapped_file = argv[4];
		if (pthread_create(&remapper, NULL, remap_sentinel_page, NULL) != 0) return 74;
	}

	sentinel_target();

	if (argc == 5 && pthread_join(remapper, NULL) != 0) return 75;
	return 0;
}
`

type nativeWriteCall struct {
	addr uint64
	data []byte
	err  error
}

// observingNativeBackend delegates every operation to the real native backend.
// The optional marker barrier only coordinates a tracee-owned mapping transition
// before the actual clear write; it does not inject or alter the backend result.
type observingNativeBackend struct {
	Backend

	mu sync.Mutex

	sentinelAddr uint64
	trap         []byte
	beforeFile   string
	unmappedFile string
	writes       []nativeWriteCall
}

func (b *observingNativeBackend) setPID(pid int) {
	b.Backend.(pidSetter).setPID(pid)
}

func (b *observingNativeBackend) execPtrace(fn func()) {
	b.Backend.(tracerExecer).execPtrace(fn)
}

func (b *observingNativeBackend) configureSentinelClear(addr uint64, trap []byte, beforeFile, unmappedFile string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sentinelAddr = addr
	b.trap = append([]byte(nil), trap...)
	b.beforeFile = beforeFile
	b.unmappedFile = unmappedFile
}

func (b *observingNativeBackend) WriteMemory(addr uint64, src []byte) error {
	b.mu.Lock()
	isClear := addr == b.sentinelAddr &&
		b.beforeFile != "" &&
		!bytes.Equal(src, b.trap)
	beforeFile := b.beforeFile
	unmappedFile := b.unmappedFile
	b.mu.Unlock()

	if isClear {
		if err := os.WriteFile(beforeFile, []byte("clear entered\n"), 0o600); err != nil {
			return fmt.Errorf("native audit marker before clear: %w", err)
		}
		if err := waitForNativePath(unmappedFile, 5*time.Second); err != nil {
			return fmt.Errorf("native audit wait for unmapped sentinel page: %w", err)
		}
	}

	err := b.Backend.WriteMemory(addr, src)
	b.mu.Lock()
	b.writes = append(b.writes, nativeWriteCall{addr: addr, data: append([]byte(nil), src...), err: err})
	b.mu.Unlock()
	return err
}

func (b *observingNativeBackend) writesAt(addr uint64) []nativeWriteCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	var writes []nativeWriteCall
	for _, write := range b.writes {
		if write.addr == addr {
			writes = append(writes, write)
		}
	}
	return writes
}

func TestNativeSentinelClearFailureFromLiveRemap(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "native_sentinel_target.c")
	binary := filepath.Join(dir, "native_sentinel_target")
	if err := os.WriteFile(source, []byte(nativeSentinelTarget), 0o600); err != nil {
		t.Fatalf("write native target source: %v", err)
	}
	if output, err := exec.Command("cc", "-O0", "-g", "-fno-omit-frame-pointer", "-fno-pie", "-no-pie",
		"-pthread", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatalf("build native target: %v\n%s", err, output)
	}

	breakpointLine := nativeTargetLine(t, nativeSentinelTarget, "PROBE_BREAKPOINT")
	runNativeSentinelControl(t, binary, source, breakpointLine)
	runNativeSentinelRemapExperiment(t, binary, source, breakpointLine, dir)
}

func runNativeSentinelControl(t *testing.T, binary, source string, breakpointLine int) {
	t.Helper()
	backend := &observingNativeBackend{Backend: newBackend()}
	d := newEngine(backend, nil)
	defer func() { _ = d.Kill() }()

	sentinelAddr := launchAndArmNativeSentinel(t, d, backend, binary, source, breakpointLine, nil)
	if event := waitNativeEvent(t, d, protocol.EventStepped); event.Kind != protocol.EventStepped {
		t.Fatalf("normal sentinel hit emitted %s", event.Kind)
	}
	if clears := backend.writesAt(sentinelAddr); !hasNativeClear(clears, false, archTrapInstruction()) {
		t.Fatalf("normal sentinel clear at 0x%x did not reach the real backend", sentinelAddr)
	}
	continueNative(t, d)
	waitNativeEvent(t, d, protocol.EventProcessExited)
	t.Logf("CONTROL: normal native sentinel clear at 0x%x succeeded and produced one Stepped event", sentinelAddr)
}

func runNativeSentinelRemapExperiment(t *testing.T, binary, source string, breakpointLine int, dir string) {
	t.Helper()
	beforeFile := filepath.Join(dir, "before-clear")
	unmappedFile := filepath.Join(dir, "unmapped")
	releaseFile := filepath.Join(dir, "release")
	mappedFile := filepath.Join(dir, "mapped")

	backend := &observingNativeBackend{Backend: newBackend()}
	d := newEngine(backend, nil)
	defer func() { _ = d.Kill() }()

	sentinelAddr := launchAndArmNativeSentinel(t, d, backend, binary, source, breakpointLine,
		[]string{beforeFile, unmappedFile, releaseFile, mappedFile})
	backend.configureSentinelClear(sentinelAddr, archTrapInstruction(), beforeFile, unmappedFile)

	first := waitNativeEvent(t, d, protocol.EventStepped)
	if first.Kind != protocol.EventStepped {
		t.Fatalf("failed native sentinel clear emitted %s", first.Kind)
	}
	if err := syscall.Kill(d.proc.pid, 0); err != nil {
		t.Fatalf("tracee was not alive after real sentinel clear failure: %v", err)
	}
	if err := waitForNativePath(unmappedFile, time.Second); err != nil {
		t.Fatalf("tracee never reported the sentinel page unmapped: %v", err)
	}
	if clears := backend.writesAt(sentinelAddr); !hasNativeClear(clears, true, archTrapInstruction()) {
		t.Fatalf("expected real WriteMemory failure restoring sentinel at 0x%x; writes=%+v", sentinelAddr, clears)
	}

	if err := os.WriteFile(releaseFile, []byte("restore\n"), 0o600); err != nil {
		t.Fatalf("release remapper: %v", err)
	}
	if err := waitForNativePath(mappedFile, 5*time.Second); err != nil {
		t.Fatalf("tracee did not restore the sentinel page: %v", err)
	}

	continueNative(t, d)
	second := waitNativeEvent(t, d, protocol.EventStepped)
	if second.Kind != protocol.EventStepped {
		t.Fatalf("re-hit sentinel emitted %s", second.Kind)
	}
	if clears := backend.writesAt(sentinelAddr); !hasNativeClear(clears, false, archTrapInstruction()) {
		t.Fatalf("re-hit did not successfully clear sentinel at 0x%x; writes=%+v", sentinelAddr, clears)
	}

	continueNative(t, d)
	waitNativeEvent(t, d, protocol.EventProcessExited)
	t.Logf("NATIVE: live tracee returned real PTRACE_POKEDATA EIO at 0x%x, emitted false Stepped, then re-hit the retained sentinel", sentinelAddr)
}

func launchAndArmNativeSentinel(
	t *testing.T,
	d *engine,
	backend *observingNativeBackend,
	binary, source string,
	breakpointLine int,
	args []string,
) uint64 {
	t.Helper()
	if err := d.Launch(binary, args, nil); err != nil {
		t.Fatalf("launch native target: %v", err)
	}
	waitNativeEvent(t, d, protocol.EventStepped)

	var sentinelAddr uint64
	if err := d.dispatch(func() error {
		if d.dw == nil {
			return errors.New("native target DWARF was not loaded")
		}
		pc, _, ok := d.dw.NextLinePC(source, breakpointLine)
		if !ok {
			return fmt.Errorf("no next source line after %s:%d", source, breakpointLine)
		}
		sentinelAddr = pc
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetBreakpoint(source, breakpointLine); err != nil {
		t.Fatalf("set initial breakpoint: %v", err)
	}
	continueNative(t, d)
	waitNativeEvent(t, d, protocol.EventBreakpointHit)
	if err := d.StepOver(); err != nil {
		t.Fatalf("StepOver to native sentinel: %v", err)
	}
	return sentinelAddr
}

func continueNative(t *testing.T, d *engine) {
	t.Helper()
	if err := d.Continue(); err != nil {
		t.Fatalf("continue: %v", err)
	}
	waitNativeEvent(t, d, protocol.EventContinued)
}

func waitNativeEvent(t *testing.T, d *engine, want protocol.EventKind) protocol.Event {
	t.Helper()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case event, ok := <-d.Events():
			if !ok {
				t.Fatalf("events closed while waiting for %s", want)
			}
			if event.Kind == protocol.EventGoroutineSnapshot {
				continue
			}
			if event.Kind == want {
				return event
			}
			t.Logf("ignoring %s while waiting for %s", event.Kind, want)
		case <-timeout.C:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func hasNativeClear(writes []nativeWriteCall, failed bool, trap []byte) bool {
	for _, write := range writes {
		if bytes.Equal(write.data, trap) {
			continue
		}
		if failed {
			if errors.Is(write.err, syscall.EIO) {
				return true
			}
			continue
		}
		if write.err == nil {
			return true
		}
	}
	return false
}

func waitForNativePath(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func nativeTargetLine(t *testing.T, source, marker string) int {
	t.Helper()
	for line, text := range bytes.Split([]byte(source), []byte("\n")) {
		if bytes.Contains(text, []byte(marker)) {
			return line + 1
		}
	}
	t.Fatalf("marker %q not found in native target", marker)
	return 0
}
