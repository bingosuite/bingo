// mach_darwin_arm64.h — Mach API wrappers for the Darwin/arm64 debugger backend.
// This header is included only by darwin_arm64.go via cgo.
// It must only be compiled on Darwin/arm64 with a macOS SDK present.

#pragma once

#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <mach/arm/thread_status.h>
#include <stdint.h>
#include <string.h>

// bingo_task_for_pid obtains the Mach task port for the given PID.
// Requires the com.apple.security.cs.debugger entitlement or SIP disabled.
static inline kern_return_t bingo_task_for_pid(int pid, mach_port_t *task_out) {
    return task_for_pid(mach_task_self(), pid, task_out);
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
// Temporarily marks the target page(s) writable with VM_PROT_COPY, then restores
// the original protection. task_suspend/task_resume quiesce the task while we
// write.
//
// NOTE: the debugger installs breakpoints with HARDWARE debug registers (see
// bingo_set_thread_hw_breakpoints), NOT by patching code, so this function is
// never used to modify executable text in normal operation — it exists only as
// a generic data-write fallback. Patching an ad-hoc-signed target's __TEXT on
// Apple Silicon invalidates its code signature and gets it AMFI-SIGKILLed on the
// next page-in, which is exactly why breakpoints are hardware, not software.
static inline kern_return_t bingo_write_memory(
    mach_port_t task, mach_vm_address_t addr,
    const void *src, mach_vm_size_t n)
{
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
    kr = mach_vm_protect(task, addr, n, FALSE, VM_PROT_READ | VM_PROT_EXECUTE);

    task_resume(task);
    return kr;
}

// bingo_set_thread_hw_breakpoints installs up to n instruction hardware
// breakpoints on ONE thread via its ARM_DEBUG_STATE64 (the AArch64 debug
// registers DBGBVR<i>/DBGBCR<i>). Each breakpoint matches an instruction fetch
// at addrs[i] in EL0 (user mode). Passing n == 0 clears all hardware
// breakpoints on the thread.
//
// Hardware breakpoints trap the CPU BEFORE the target instruction executes
// (delivered as a SIGTRAP that is visible to wait4) WITHOUT modifying any target
// memory. That is the whole point on Apple Silicon: no code page is patched, so
// the ad-hoc code signature stays valid (no AMFI kill) and there is no i-cache
// coherency problem. The PC on the resulting stop is exactly addrs[i].
// __mdscr_el1 is left 0 here (single-step is driven separately via PT_STEP).
static inline kern_return_t bingo_set_thread_hw_breakpoints(
    mach_port_t thread, const uint64_t *addrs, int n)
{
    arm_debug_state64_t dbg;
    memset(&dbg, 0, sizeof(dbg));
    if (n > 16) n = 16;
    for (int i = 0; i < n; i++) {
        dbg.__bvr[i] = (__uint64_t)addrs[i];
        // DBGBCR: E=1 (enable), PMC=0b10 (match EL0/user only),
        // BAS=0b1111 (all four instruction bytes).
        dbg.__bcr[i] = 0x1u | (0x2u << 1) | (0xfu << 5);
    }
    return thread_set_state(thread, ARM_DEBUG_STATE64,
        (thread_state_t)&dbg, ARM_DEBUG_STATE64_COUNT);
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
//
// task_threads inserts a *fresh send right* into our IPC space for every thread
// it returns, so the caller must both (a) mach_port_deallocate each individual
// thread port and (b) vm_deallocate the array itself. Deallocating only the
// array (the array's backing memory) leaks one thread-port send right per thread
// per call — at SIGURG re-arm frequency that exhausts our Mach port name space
// within seconds and makes subsequent task_threads / thread_set_state calls fail,
// which silently disarms every hardware breakpoint (=> the tracee runs away and
// wait4 hangs). bingo_free_thread_list does both halves correctly.
static inline kern_return_t bingo_thread_list(
    mach_port_t task,
    thread_act_port_array_t *threads_out,
    mach_msg_type_number_t *count_out)
{
    return task_threads(task, threads_out, count_out);
}

// bingo_free_thread_list releases everything bingo_thread_list handed back: one
// send right per thread port, then the array allocation. Must be called for
// every successful bingo_thread_list to keep the port-right count balanced.
static inline void bingo_free_thread_list(
    thread_act_port_array_t threads,
    mach_msg_type_number_t count)
{
    if (threads == NULL) return;
    for (mach_msg_type_number_t i = 0; i < count; i++) {
        mach_port_deallocate(mach_task_self(), threads[i]);
    }
    vm_deallocate(mach_task_self(), (vm_address_t)threads,
        count * sizeof(mach_port_t));
}

// bingo_thread_suspend increments the Mach suspend count of a single thread.
// A suspended thread will not run even when the process is ptrace-resumed
// (PT_CONTINUE / PT_STEP), which lets us isolate one thread for single-stepping.
static inline kern_return_t bingo_thread_suspend(mach_port_t thread) {
    return thread_suspend(thread);
}

// bingo_thread_resume fully drains a thread's Mach suspend count back to zero.
// Symmetric with bingo_thread_suspend for our one-level suspensions, but robust
// if the count is >1 (e.g. an overlapping task_suspend/resume bumped it).
static inline kern_return_t bingo_thread_resume(mach_port_t thread) {
    struct thread_basic_info info;
    mach_msg_type_number_t count = THREAD_BASIC_INFO_COUNT;
    kern_return_t kr = thread_info((thread_t)thread, THREAD_BASIC_INFO,
        (thread_info_t)&info, &count);
    if (kr != KERN_SUCCESS) return kr;
    for (int i = 0; i < info.suspend_count; i++) {
        kr = thread_resume(thread);
        if (kr != KERN_SUCCESS) return kr;
    }
    return KERN_SUCCESS;
}

// bingo_resume_all_threads drains the Mach suspend count of every thread in the
// tracee task to zero. Used before killing so no Mach-suspended thread blocks
// termination. Best-effort: individual thread failures (e.g. a thread exited
// mid-enumeration) are ignored so the caller can still proceed to kill.
static inline kern_return_t bingo_resume_all_threads(int pid) {
    mach_port_t task = MACH_PORT_NULL;
    kern_return_t kr = task_for_pid(mach_task_self(), pid, &task);
    if (kr != KERN_SUCCESS) return kr;

    thread_act_port_array_t threads = NULL;
    mach_msg_type_number_t count = 0;
    kr = task_threads(task, &threads, &count);
    if (kr != KERN_SUCCESS) {
        mach_port_deallocate(mach_task_self(), task);
        return kr;
    }

    for (mach_msg_type_number_t i = 0; i < count; i++) {
        bingo_thread_resume(threads[i]);
    }
    bingo_free_thread_list(threads, count);
    mach_port_deallocate(mach_task_self(), task);
    return KERN_SUCCESS;
}

