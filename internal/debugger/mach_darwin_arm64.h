// mach_darwin_arm64.h — Mach API wrappers for the Darwin/arm64 debugger backend.
// This header is included only by darwin_arm64.go via cgo.
// It must only be compiled on Darwin/arm64 with a macOS SDK present.

#pragma once

#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <mach/vm_attributes.h>
#include <mach/arm/thread_status.h>
#include <stdint.h>

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

// bingo_write_memory writes n bytes from src into the task's address space,
// mirroring LLDB debugserver's MachVMMemory::WriteRegion and Delve's
// write_memory (pkg/proc/native/threads_darwin.c) — the proven technique for
// planting software breakpoints (BRK) on Apple Silicon.
//
// Two details are load-bearing on Darwin/arm64:
//
//   - Instruction-cache flush. mach_vm_write updates memory through the DATA
//     side, but the tracee's cores may still hold the ORIGINAL instruction in
//     their L1 I-caches; without an explicit flush a freshly written BRK can be
//     intermittently ineffective and the tracee executes straight through it.
//     vm_machine_attribute(MATTR_CACHE, MATTR_VAL_CACHE_FLUSH) performs the
//     cross-process DC-clean-to-PoU + IC-invalidate (broadcast to the inner-
//     shareable domain, so every core sees it). Use vm_machine_attribute, NOT
//     mach_vm_machine_attribute: only the former is wired to the arm64 pmap
//     cache-flush path; the mach_vm_ variant returns KERN_SUCCESS but does not
//     reliably invalidate the tracee's I-caches.
//
//   - VM_PROT_COPY when making the range writable. Text pages are file-backed
//     and code-signed with max_protection R|X. Requesting VM_PROT_COPY forces a
//     private, anonymous copy-on-write shadow of the page, so the patch lands in
//     an unsigned private copy rather than the shared, signed file-backed page.
//     This is exactly what Delve and debugserver do (both pass VM_PROT_WRITE|
//     VM_PROT_COPY|VM_PROT_READ unconditionally). A CS_DEBUGGED (ptrace-traced)
//     task is in fact permitted to patch its own text directly — the kernel logs
//     "AMFI: code signature validation failed" for the modified page but does
//     NOT kill the process (verified: runs complete cleanly while these logs
//     fire continuously) — so COPY is not strictly required for correctness
//     here. We use it because it is the reference-correct technique and keeps
//     the write off the shared signed page entirely; we try COPY first and fall
//     back to plain R|W only if COPY is rejected outright (for non-text pages).
//
// Do NOT task_suspend. The engine only writes memory while the tracee is fully
// ptrace-stopped (no thread is running), exactly as debugserver writes to an
// inferior stopped on a Mach exception. An extra task_suspend/resume is
// redundant and perturbs the signal/scheduler state the step-off path depends on.
static inline kern_return_t bingo_write_memory(
    mach_port_t task, mach_vm_address_t addr,
    const void *src, mach_vm_size_t n)
{
    // Query the region so we can restore its exact original protection afterwards.
    mach_vm_address_t region_addr = addr;
    mach_vm_size_t region_size = n;
    vm_region_basic_info_data_64_t info;
    mach_msg_type_number_t info_count = VM_REGION_BASIC_INFO_COUNT_64;
    mach_port_t object_name = MACH_PORT_NULL;
    vm_prot_t orig_prot = VM_PROT_READ | VM_PROT_EXECUTE;
    if (mach_vm_region(task, &region_addr, &region_size, VM_REGION_BASIC_INFO_64,
                       (vm_region_info_t)&info, &info_count, &object_name)
            == KERN_SUCCESS) {
        orig_prot = info.protection;
    }

    // Make the range writable via a private copy-on-write shadow (VM_PROT_COPY).
    // This is mandatory on Apple Silicon: it keeps the write off the shared,
    // code-signed text page so AMFI can't kill the tracee for a hash mismatch.
    // Fall back to plain R|W only if COPY is rejected outright.
    kern_return_t kr = mach_vm_protect(task, addr, n, FALSE,
        VM_PROT_READ | VM_PROT_WRITE | VM_PROT_COPY);
    if (kr != KERN_SUCCESS) {
        kr = mach_vm_protect(task, addr, n, FALSE,
            VM_PROT_READ | VM_PROT_WRITE);
    }
    if (kr != KERN_SUCCESS) {
        return kr;
    }

    kr = mach_vm_write(task, addr, (vm_offset_t)src, (mach_msg_type_number_t)n);
    if (kr == KERN_SUCCESS) {
        // Flush I-cache for the range while it is still writable, then restore
        // the original protection.
        vm_machine_attribute_val_t flush = MATTR_VAL_CACHE_FLUSH;
        vm_machine_attribute(task, addr, n, MATTR_CACHE, &flush);
    }
    mach_vm_protect(task, addr, n, FALSE, orig_prot);
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
// The caller must vm_deallocate the returned threads array and
// mach_port_deallocate every returned thread port (each is a send right).
static inline kern_return_t bingo_thread_list(
    mach_port_t task,
    thread_act_port_array_t *threads_out,
    mach_msg_type_number_t *count_out)
{
    return task_threads(task, threads_out, count_out);
}

// bingo_set_thread_single_step arms (on != 0) or disarms hardware single-step
// on ONE specific thread by toggling MDSCR_EL1.SS (bit 0) in its
// ARM_DEBUG_STATE64. This is the per-thread mechanism used by LLDB's
// debugserver on Apple Silicon.
//
// It exists because ptrace(PT_STEP) is useless on multithreaded Darwin: XNU's
// PT_STEP arms single-step on get_firstthread(task) only (bsd/kern/mach_process.c
// -> thread_setsinglestep in osfmk/arm64/bsd_arm64.c), which is almost never the
// thread that hit the breakpoint. thread_setsinglestep writes the same
// per-thread mdscr_el1 debug state we set here, so arming it directly on the
// trapped thread lets us single-step exactly that thread.
//
// Existing bcr/bvr/wcr/wvr (hardware breakpoints/watchpoints) are preserved by
// reading the current state first; on read failure we start from a zeroed state.
//
// Reallocation on arm is REQUIRED for correctness, not an optimization. XNU's
// return-to-user path (osfmk/arm64/locore.s) only reloads a thread's debug
// registers when the live per-CPU debug pointer differs from the thread's:
//
//     ldr x1, [x4, CPU_USER_DEBUG]   // cpu_user_debug (live on this CPU)
//     ldr x0, [x3, ACT_DEBUGDATA]    // thread->machine.DebugData
//     cmp x0, x1
//     beq L_skip_user_set_debug_state   // POINTER compare -> skip reload
//     bl  EXT(arm_debug_set)            // else apply thread debug state
//
// arm_debug_set64 (osfmk/arm64/pcb.c) is what actually writes MDSCR_EL1.SS into
// the hardware AND sets PSTATE.SS in the thread's saved CPSR (via
// mask_user_saved_state_cpsr) — the two bits the ARM software-step state machine
// needs. We set the SS bit out-of-band on an off-CPU thread, mutating the
// CONTENTS of its debug struct while its POINTER stays the same across arm/disarm
// (find_or_allocate_debug_state64 reuses an existing allocation). If that thread
// is then scheduled onto a CPU whose cpu_user_debug already points at the very
// same struct (with SS currently clear in hardware), the compare matches, the
// reload is skipped, and our freshly-set SS bit never reaches the CPU: the thread
// runs free, no step exception fires, and the next wait4 blocks forever. Whether
// the target lands on such a CPU depends on the scheduler, which is why the hang
// is intermittent (~50%).
//
// To defeat the skip we force the struct to be reallocated on every arm. Writing
// an all-zero (disabled) debug state makes machine_thread_set_state call
// free_debug_state(), which sets thread->machine.DebugData = NULL. The following
// enabled write then goes through find_or_allocate_debug_state64 and zalloc's a
// brand-new struct. A never-before-loaded pointer cannot equal any CPU's
// cpu_user_debug (the live one is retained, so its storage can't be recycled),
// so the return-to-user path always takes the reload branch and applies SS.
static inline kern_return_t bingo_set_thread_single_step(mach_port_t thread, int on) {
    arm_debug_state64_t ds = {0};
    mach_msg_type_number_t count = ARM_DEBUG_STATE64_COUNT;
    (void)thread_get_state(thread, ARM_DEBUG_STATE64, (thread_state_t)&ds, &count);
    if (on) {
        // Free the current debug state (DebugData -> NULL) so the enabled write
        // below allocates a fresh struct with a new pointer, forcing a reload.
        arm_debug_state64_t zero = {0};
        (void)thread_set_state(thread, ARM_DEBUG_STATE64,
            (thread_state_t)&zero, ARM_DEBUG_STATE64_COUNT);
        ds.__mdscr_el1 |= 1ULL;      // MDSCR_EL1.SS
    } else {
        ds.__mdscr_el1 &= ~1ULL;
    }
    return thread_set_state(thread, ARM_DEBUG_STATE64,
        (thread_state_t)&ds, ARM_DEBUG_STATE64_COUNT);
}

// bingo_port_deallocate releases one user reference to a Mach port name in our
// own IPC space (used to release thread send-rights returned by task_threads).
static inline kern_return_t bingo_port_deallocate(mach_port_t port) {
    return mach_port_deallocate(mach_task_self(), port);
}

// bingo_thread_suspend / bingo_thread_resume adjust a Mach thread's suspend
// count. During a single-step we suspend every thread except the one being
// stepped so the whole task can be ptrace-resumed while only the target thread
// actually runs — mirroring how debugserver isolates the stepping thread. This
// keeps concurrent threads (and their SIGURG preemption signals) from perturbing
// the ARM software-step state machine, which otherwise intermittently swallows
// the step exception and hangs wait4.
static inline kern_return_t bingo_thread_suspend(mach_port_t thread) {
    return thread_suspend(thread);
}
static inline kern_return_t bingo_thread_resume(mach_port_t thread) {
    return thread_resume(thread);
}
