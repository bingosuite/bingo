// mach_darwin_arm64.h — Mach API wrappers for the Darwin/arm64 debugger backend.
// This header is included only by darwin_arm64.go via cgo.
// It must only be compiled on Darwin/arm64 with a macOS SDK present.

#pragma once

#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <mach/arm/thread_status.h>
#include <mach/vm_attributes.h>
#include <stdint.h>
#include <string.h>
#include <stdlib.h>
#include <spawn.h>
#include <errno.h>

extern char **environ;

// MDSCR_EL1.SS (bit 0) is the AArch64 "software step" enable. It is not
// declared in the userspace SDK headers, so define it here.
#ifndef MDSCR_SS
#define MDSCR_SS 0x1u
#endif

// bingo_task_for_pid obtains the Mach task port for the given PID.
// Requires the com.apple.security.cs.debugger entitlement or SIP disabled.
static inline kern_return_t bingo_task_for_pid(int pid, mach_port_t *task_out) {
    return task_for_pid(mach_task_self(), pid, task_out);
}

// bingo_posix_spawn_suspended launches path (argv/envp) with the task created
// suspended (POSIX_SPAWN_START_SUSPENDED). The image is exec'd but no user code
// runs, so the debugger can PT_ATTACH on the post-exec image — that is what
// makes the kernel run cs_allow_invalid() (sets CS_DEBUGGED, clears
// CS_KILL|CS_HARD, enables the vm_map W^X bypass) on the FINAL image, which is
// required to patch software breakpoints on Apple Silicon without the kernel
// SIGKILLing the tracee. Returns the child pid, or -errno on failure.
static inline int bingo_posix_spawn_suspended(
    const char *path, char *const argv[], char *const envp[])
{
    posix_spawnattr_t attr;
    if (posix_spawnattr_init(&attr) != 0) {
        return -errno;
    }
    (void)posix_spawnattr_setflags(&attr, POSIX_SPAWN_START_SUSPENDED);
    pid_t pid = 0;
    int rc = posix_spawn(&pid, path, NULL, &attr, argv, envp);
    posix_spawnattr_destroy(&attr);
    if (rc != 0) {
        return -rc;
    }
    return (int)pid;
}

// bingo_task_drain_suspend lifts EVERY outstanding task-level Mach suspend on
// pid (calling task_resume until the count reaches 0). The launch path spawns
// the tracee POSIX_SPAWN_START_SUSPENDED (task suspend count 1) and expects
// PT_ATTACH to lift that hold; on Apple Silicon that lift is racy, and when it
// is missed the tracee stays frozen at _dyld_start (threads read suspend=0
// because the hold is at the TASK level) and the first Continue after launch
// never runs it. Draining here guarantees the hold is gone; the pending
// attach-SIGSTOP is what actually stops the tracee at the entry point, so
// resuming to 0 does not let it run away. Returns the number of resumes done,
// or -1 on task_for_pid error.
static inline int bingo_task_drain_suspend(int pid) {
    mach_port_t task = MACH_PORT_NULL;
    if (task_for_pid(mach_task_self(), pid, &task) != KERN_SUCCESS) {
        return -1;
    }
    int resumed = 0;
    for (;;) {
        struct task_basic_info info;
        mach_msg_type_number_t count = TASK_BASIC_INFO_COUNT;
        if (task_info(task, TASK_BASIC_INFO, (task_info_t)&info, &count) != KERN_SUCCESS) {
            break;
        }
        if (info.suspend_count <= 0) {
            break;
        }
        if (task_resume(task) != KERN_SUCCESS) {
            break;
        }
        resumed++;
        if (resumed > 8) { // safety bound; never seen >1 in practice
            break;
        }
    }
    mach_port_deallocate(mach_task_self(), task);
    return resumed;
}

// bingo_get_registers reads ARM_THREAD_STATE64 for the given thread port,
// extracting the four registers the engine cares about.
static inline kern_return_t bingo_get_registers(
    mach_port_t thread,
    uint64_t *pc, uint64_t *sp, uint64_t *fp, uint64_t *g)
{
    arm_thread_state64_t state;
    mach_msg_type_number_t count = ARM_THREAD_STATE64_COUNT;
    kern_return_t kr = thread_get_state(
        thread, ARM_THREAD_STATE64,
        (thread_state_t)&state, &count);
    if (kr != KERN_SUCCESS) return kr;
    *pc = (uint64_t)state.__pc;
    *sp = (uint64_t)state.__sp;
    *fp = (uint64_t)state.__fp;     // X29 = frame pointer
    *g  = (uint64_t)state.__x[28]; // X28 = Go's goroutine pointer
    return KERN_SUCCESS;
}

// bingo_set_registers writes pc/sp/fp/g back into ARM_THREAD_STATE64,
// reading the full state first to preserve all other registers.
static inline kern_return_t bingo_set_registers(
    mach_port_t thread,
    uint64_t pc, uint64_t sp, uint64_t fp, uint64_t g)
{
    arm_thread_state64_t state;
    mach_msg_type_number_t count = ARM_THREAD_STATE64_COUNT;
    kern_return_t kr = thread_get_state(
        thread, ARM_THREAD_STATE64,
        (thread_state_t)&state, &count);
    if (kr != KERN_SUCCESS) return kr;
    state.__pc     = (__uint64_t)pc;
    state.__sp     = (__uint64_t)sp;
    state.__fp     = (__uint64_t)fp;
    state.__x[28]  = (__uint64_t)g;
    count = ARM_THREAD_STATE64_COUNT;
    return thread_set_state(
        thread, ARM_THREAD_STATE64,
        (thread_state_t)&state, count);
}

// bingo_read_memory reads n bytes from the task's address space at addr.
// Uses mach_vm_read_overwrite, which works on arm64 unlike PT_READ_D.
static inline kern_return_t bingo_read_memory(
    mach_port_t task, mach_vm_address_t addr,
    void *dst, mach_vm_size_t n)
{
    mach_vm_size_t out_size = 0;
    return mach_vm_read_overwrite(task, addr, n,
        (mach_vm_address_t)dst, &out_size);
}

// bingo_write_memory writes n bytes from src into the task's address space.
// Temporarily marks the target page(s) writable with VM_PROT_COPY so we can
// patch read-only text segments (e.g. to install breakpoints), then restores
// execute permission.
//
// Icache coherency on Apple Silicon (ARM64): after patching instruction bytes
// we must clean the D-cache to the point of unification (so the freshly-written
// bytes reach unified memory) and THEN invalidate the I-cache (so cores refetch
// the new bytes from PoU rather than a stale line). MATTR_VAL_CACHE_SYNC alone
// can invalidate the I-cache before the write has drained past PoU, leaving a
// stale instruction line, so we issue the two attribute calls explicitly in
// order.
//
// We deliberately do NOT wrap the write in task_suspend/task_resume. bingo only
// ever writes memory while the tracee is ptrace-stopped (Darwin ptrace stops
// are process-wide, so every thread is already quiesced), and the explicit
// D-cache/I-cache flush provides the coherency the task_resume pipeline drain
// used to. task_suspend/task_resume use a suspend count independent of ptrace,
// and on Apple Silicon a task_resume issued near a ptrace state transition
// (PT_CONTINUE / PT_STEP on the wait4 thread) can be dropped, pinning the task
// at suspend_count 1 — a pinned task never runs, so the next wait4 blocks
// forever and the debugger wedges. Dropping the suspend/resume entirely removes
// that race; single-step isolation uses per-THREAD suspends (never task-level).
static inline kern_return_t bingo_write_memory(
    mach_port_t task, mach_vm_address_t addr,
    const void *src, mach_vm_size_t n)
{
    kern_return_t kr = mach_vm_protect(task, addr, n, FALSE,
        VM_PROT_READ | VM_PROT_WRITE | VM_PROT_COPY);
    if (kr != KERN_SUCCESS) {
        return kr;
    }
    kr = mach_vm_write(task, addr,
        (vm_offset_t)src, (mach_msg_type_number_t)n);
    if (kr != KERN_SUCCESS) {
        return kr;
    }
    kr = mach_vm_protect(task, addr, n, FALSE,
        VM_PROT_READ | VM_PROT_EXECUTE);

    vm_machine_attribute_val_t flush = MATTR_VAL_DCACHE_FLUSH;
    mach_vm_machine_attribute(task, addr, n, MATTR_CACHE, &flush);
    flush = MATTR_VAL_ICACHE_FLUSH;
    mach_vm_machine_attribute(task, addr, n, MATTR_CACHE, &flush);

    return kr;
}

// bingo_find_macho_load_addr scans the task's virtual address space from
// address 0 upward and returns the address of the first executable region
// whose first four bytes are the 64-bit Mach-O magic (0xFEEDFACF).
//
// This finds the main executable's actual __TEXT vmaddr even before dyld has
// run (i.e. at the very first ptrace stop after exec), because the kernel maps
// the binary into memory before transferring control to dyld.  The ASLR slide
// is then:  slide = *load_addr_out - preferred_text_vmaddr_from_file.
static inline kern_return_t bingo_find_macho_load_addr(
    mach_port_t task, mach_vm_address_t *load_addr_out)
{
    mach_vm_address_t addr = 0;
    mach_vm_size_t    size = 0;
    mach_port_t       obj  = MACH_PORT_NULL;
    vm_region_basic_info_data_64_t info;
    mach_msg_type_number_t count;

    for (;;) {
        count = VM_REGION_BASIC_INFO_COUNT_64;
        if (obj != MACH_PORT_NULL) {
            mach_port_deallocate(mach_task_self(), obj);
            obj = MACH_PORT_NULL;
        }
        kern_return_t kr = mach_vm_region(task, &addr, &size,
            VM_REGION_BASIC_INFO_64,
            (vm_region_info_t)&info, &count, &obj);
        if (kr != KERN_SUCCESS) break;

        if (info.protection & VM_PROT_EXECUTE) {
            uint32_t magic = 0;
            mach_vm_size_t out_sz = 0;
            kr = mach_vm_read_overwrite(task, addr, sizeof(magic),
                                        (mach_vm_address_t)&magic, &out_sz);
            if (kr == KERN_SUCCESS && magic == 0xFEEDFACFu) {
                if (obj != MACH_PORT_NULL)
                    mach_port_deallocate(mach_task_self(), obj);
                *load_addr_out = addr;
                return KERN_SUCCESS;
            }
        }
        addr += size;
    }
    if (obj != MACH_PORT_NULL)
        mach_port_deallocate(mach_task_self(), obj);
    return KERN_FAILURE;
}

// bingo_thread_list enumerates all threads in task.
// The caller must vm_deallocate the returned threads array.
static inline kern_return_t bingo_thread_list(
    mach_port_t task,
    thread_act_port_array_t *threads_out,
    mach_msg_type_number_t *count_out)
{
    return task_threads(task, threads_out, count_out);
}

// bingo_set_single_step enables (enable != 0) or disables the AArch64 hardware
// software-step for a single thread by toggling MDSCR_EL1.SS in its debug
// state. XNU honours this bit set via thread_set_state(ARM_DEBUG_STATE64) and
// engages the single-step state machine for that thread only on its next return
// to EL0 — this is exactly how lldb-debugserver single-steps on Apple Silicon.
//
// We deliberately write a zeroed debug state (only MDSCR_EL1.SS may be set):
// the debugger installs breakpoints as software BRK traps, never via the ARM
// hardware breakpoint/watchpoint registers, so there is nothing else to
// preserve. Disabling clears every bit, so XNU frees the thread's debug state.
static inline kern_return_t bingo_set_single_step(mach_port_t thread, int enable) {
    arm_debug_state64_t ds;
    memset(&ds, 0, sizeof(ds));
    if (enable) {
        ds.__mdscr_el1 |= MDSCR_SS;
    }
    return thread_set_state(thread, ARM_DEBUG_STATE64,
        (thread_state_t)&ds, ARM_DEBUG_STATE64_COUNT);
}

// bingo_thread_suspend / bingo_thread_resume adjust a single thread's Mach
// suspend count. This is independent of the task-level ptrace stop, so it lets
// us hold every thread except the one being single-stepped while the task is
// resumed for that step.
static inline kern_return_t bingo_thread_suspend(mach_port_t thread) {
    return thread_suspend((thread_act_t)thread);
}

static inline kern_return_t bingo_thread_resume(mach_port_t thread) {
    return thread_resume((thread_act_t)thread);
}

// bingo_port_deallocate drops one user reference on a send right, matching the
// ref that task_for_pid / task_threads inserted. Used to release the cached
// task port when the tracee is replaced.
static inline kern_return_t bingo_port_deallocate(mach_port_t name) {
    return mach_port_deallocate(mach_task_self(), name);
}
