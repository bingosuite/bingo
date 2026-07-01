// mach_darwin_arm64.h — Mach API wrappers for the Darwin/arm64 debugger backend.
// This header is included only by darwin_arm64.go via cgo.
// It must only be compiled on Darwin/arm64 with a macOS SDK present.

#pragma once

#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <mach/arm/thread_status.h>
#include <stdint.h>
#include <unistd.h>

// bingo_task_for_pid obtains the Mach task port for the given PID.
// Requires the com.apple.security.cs.debugger entitlement or SIP disabled.
static inline kern_return_t bingo_task_for_pid(int pid, mach_port_t *task_out) {
    return task_for_pid(mach_task_self(), pid, task_out);
}

// bingo_get_registers_once reads ARM_THREAD_STATE64 for the given thread port,
// extracting the four registers the engine cares about.
static inline kern_return_t bingo_get_registers_once(
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

// bingo_set_registers_once writes pc/sp/fp/g back into ARM_THREAD_STATE64,
// reading the full state first to preserve all other registers.
static inline kern_return_t bingo_set_registers_once(
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

// bingo_get_registers / bingo_set_registers wrap the thread-state accessors
// with a bounded retry on KERN_ABORTED. thread_get_state / thread_set_state can
// transiently return KERN_ABORTED when the kernel is mid-operation on the
// thread — e.g. right after a single-step exception is delivered (the target
// just trapped and the kernel is still tearing down its debug-state fault), or
// while an asynchronous thread_suspend/resume is settling the thread on/off a
// CPU. The thread is ptrace-stopped whenever we read or write its GPRs, so it
// cannot run free during the retries; the state op just needs the kernel to
// finish. LLDB debugserver retries its GPR accessors (MachThread GPR get/set)
// on KERN_ABORTED for exactly this reason. Bound 50 * 200us = 10ms worst case,
// only ever elapsed on the rare error path.
static inline kern_return_t bingo_get_registers(
    mach_port_t thread,
    uint64_t *pc, uint64_t *sp, uint64_t *fp, uint64_t *g)
{
    kern_return_t kr = KERN_SUCCESS;
    for (int i = 0; i < 50; i++) {
        kr = bingo_get_registers_once(thread, pc, sp, fp, g);
        if (kr != KERN_ABORTED) return kr;
        usleep(200);
    }
    return kr;
}

static inline kern_return_t bingo_set_registers(
    mach_port_t thread,
    uint64_t pc, uint64_t sp, uint64_t fp, uint64_t g)
{
    kern_return_t kr = KERN_SUCCESS;
    for (int i = 0; i < 50; i++) {
        kr = bingo_set_registers_once(thread, pc, sp, fp, g);
        if (kr != KERN_ABORTED) return kr;
        usleep(200);
    }
    return kr;
}

// bingo_set_single_step arms/disarms ARM64 hardware software-step on ONE
// specific Mach thread by toggling MDSCR_EL1.SS (bit 0) in the thread's debug
// state via thread_set_state(ARM_DEBUG_STATE64). This is EXACTLY what LLDB
// debugserver's DNBArchMachARM64::EnableHardwareSingleStep does (it sets only
// __mdscr_el1 |= SS_ENABLE and calls SetDBGState), and what XNU's own
// thread_setsinglestep does for PT_STEP (bsd/kern/mach_process.c → sets only
// DebugData->mdscr_el1 |= MDSCR_SS).
//
// We deliberately do NOT program PSTATE.SS (CPSR bit 21) ourselves. XNU sets it
// for us: when arm_debug_set64 (osfmk/arm64/pcb.c) applies a debug state whose
// mdscr_el1 has SS set, it runs `mask_user_saved_state_cpsr(current_thread()->
// machine.upcb, PSR64_SS, 0)`, i.e. it sets PSTATE.SS on the stepping thread's
// saved CPSR on the return-to-user path. Setting CPSR.SS ourselves in addition
// was the bug: it put the ARM software-step state machine into an inconsistent
// active-pending/active-not-pending combination on some scheduling interleavings
// (the target came back either not stepping — running free past the removed
// trap until it deadlocked on a frozen sibling — or double-stepping). Matching
// debugserver exactly (MDSCR only) makes the step fire deterministically.
//
// Why this and not Darwin PT_STEP: PT_STEP is per-PROCESS. In XNU
// (bsd/kern/mach_process.c) it calls thread_setsinglestep(get_firstthread(task))
// — it sets the step bit on the task's FIRST thread, never the thread that
// trapped. When the Go goroutine that hit the breakpoint is running on some
// other thread, PT_STEP steps the wrong (often idle) thread; the real thread
// runs free past the temporarily-removed trap and never traps, so Wait() hangs.
//
// Clearing writes SS=0; a zeroed mdscr frees the thread's debug state
// (osfmk/arm64/pcb.c). Ref: LLDB debugserver arm64
// DNBArchMachARM64::EnableHardwareSingleStep, XNU thread_setsinglestep /
// arm_debug_set64.
static inline kern_return_t bingo_set_single_step_once(mach_port_t thread, int enable) {
    arm_debug_state64_t dbg;
    mach_msg_type_number_t dcount = ARM_DEBUG_STATE64_COUNT;
    // GET always succeeds for a 64-bit thread, returning a zeroed state when the
    // thread has no prior debug state (osfmk/arm64/status.c bzero path).
    kern_return_t kr = thread_get_state(
        thread, ARM_DEBUG_STATE64, (thread_state_t)&dbg, &dcount);
    if (kr != KERN_SUCCESS) return kr;
    if (enable) {
        dbg.__mdscr_el1 |= 1ULL;   // MDSCR_EL1.SS — enable software single step
    } else {
        dbg.__mdscr_el1 &= ~1ULL;  // clear SS; zeroed state frees debug state
    }
    return thread_set_state(
        thread, ARM_DEBUG_STATE64, (thread_state_t)&dbg, ARM_DEBUG_STATE64_COUNT);
}

// bingo_set_single_step wraps the arming sequence with a bounded retry on
// KERN_ABORTED. thread_get_state / thread_set_state can transiently return
// KERN_ABORTED when the kernel is mid-operation on the thread — specifically
// right after PT_CONTINUE, which pokes the task's FIRST thread to clear its
// per-process single-step bit (XNU bsd/kern/mach_process.c), and while an
// asynchronous thread_suspend is still settling the target out of a CPU. The
// target thread is thread_suspend'd across this call (SingleStep freezes every
// thread before PT_CONTINUE and only resumes the target afterwards), so it
// cannot run free during the retries — it is genuinely quiesced and the state
// op just needs the kernel to finish. LLDB debugserver retries its thread-state
// accessors on KERN_ABORTED for exactly this reason. The bound (50 * 200us =
// 10ms worst case) only ever elapses on the rare error path.
static inline kern_return_t bingo_set_single_step(mach_port_t thread, int enable) {
    kern_return_t kr = KERN_SUCCESS;
    for (int i = 0; i < 50; i++) {
        kr = bingo_set_single_step_once(thread, enable);
        if (kr != KERN_ABORTED) {
            return kr;
        }
        usleep(200);
    }
    return kr;
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
// Icache coherency on Apple Silicon (ARM64):
// mach_vm_machine_attribute(MATTR_CACHE, MATTR_VAL_ICACHE_FLUSH) is a no-op
// on Apple Silicon — the kernel returns KERN_NOT_SUPPORTED. The correct
// approach is to wrap the write with task_suspend + task_resume: the resume
// call drains the instruction pipeline for all threads in the task, ensuring
// the CPU sees the new bytes. task_suspend/resume use an independent suspend
// count from ptrace, so this is safe to call while the process is
// ptrace-stopped (the ptrace stop is unaffected).
static inline kern_return_t bingo_write_memory(
    mach_port_t task, mach_vm_address_t addr,
    const void *src, mach_vm_size_t n)
{
    // Suspend the task so all threads are quiesced while we patch memory.
    // This also causes task_resume to flush the instruction pipeline.
    task_suspend(task);

    kern_return_t kr = mach_vm_protect(task, addr, n, FALSE,
        VM_PROT_READ | VM_PROT_WRITE | VM_PROT_COPY);
    if (kr != KERN_SUCCESS) {
        task_resume(task);
        return kr;
    }
    kr = mach_vm_write(task, addr,
        (vm_offset_t)src, (mach_msg_type_number_t)n);
    if (kr != KERN_SUCCESS) {
        task_resume(task);
        return kr;
    }
    kr = mach_vm_protect(task, addr, n, FALSE,
        VM_PROT_READ | VM_PROT_EXECUTE);

    // Resume lifts the task suspension and flushes the instruction pipeline
    // for all threads, ensuring the CPU fetches the new bytes on next execute.
    task_resume(task);
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

// bingo_thread_suspend / bingo_thread_resume wrap Mach thread_suspend /
// thread_resume. They freeze/unfreeze one Mach thread for the duration of a
// single step so that ONLY the target thread advances (mirrors LLDB
// debugserver, which suspends sibling threads while stepping one). Freezing the
// runtime threads also stops Go's sysmon from flooding SIGURG during the step,
// which would otherwise interrupt every resume before the target can take its
// single instruction.
static inline kern_return_t bingo_thread_suspend(mach_port_t thread) {
    return thread_suspend(thread);
}
static inline kern_return_t bingo_thread_resume(mach_port_t thread) {
    return thread_resume(thread);
}
// bingo_task_suspend_count returns the Mach suspend count of a task via
// task_info(TASK_BASIC_INFO). Used to measure how many times the BSD ptrace
// layer has left the tracee task suspended so the surplus can be drained
// (all threads stay frozen and wait4 blocks while the count is > 0). Returns
// -1 on error.
static inline int bingo_task_suspend_count(mach_port_t task) {
    struct task_basic_info info;
    mach_msg_type_number_t count = TASK_BASIC_INFO_COUNT;
    kern_return_t kr = task_info(task, TASK_BASIC_INFO,
        (task_info_t)&info, &count);
    if (kr != KERN_SUCCESS) return -1;
    return (int)info.suspend_count;
}

// bingo_task_resume decrements a task's Mach suspend count (task_resume).
// Used to drain surplus task-level suspends that the BSD ptrace layer leaks on
// a multithreaded tracee (it performs at most one task_resume per PT_CONTINUE,
// but concurrent thread stops can task_suspend the task more than once).
static inline kern_return_t bingo_task_resume(mach_port_t task) {
    return task_resume(task);
}
