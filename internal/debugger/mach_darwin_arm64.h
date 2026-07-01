// mach_darwin_arm64.h — Mach API wrappers for the Darwin/arm64 debugger backend.
// This header is included only by darwin_arm64.go via cgo.
// It must only be compiled on Darwin/arm64 with a macOS SDK present.

#pragma once

#include <mach/mach.h>
#include <mach/mach_vm.h>
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

// bingo_thread_suspend / bingo_thread_resume adjust one thread's Mach suspend
// count. These are independent of the ptrace task-level stop, so they let us
// hold every thread EXCEPT the one we want to single-step over a breakpoint.
static inline kern_return_t bingo_thread_suspend(mach_port_t thread) {
    return thread_suspend(thread);
}

static inline kern_return_t bingo_thread_resume(mach_port_t thread) {
    return thread_resume(thread);
}

// bingo_set_single_step turns ARMv8 hardware software-step on/off for ONE
// specific thread, independent of ptrace PT_STEP (which is per-process and
// applies single-step to the kernel's first task thread, not the thread that
// hit the breakpoint). This is the same mechanism LLDB debugserver uses
// (DNBArchMachARM64::EnableHardwareSingleStep):
//   - MDSCR_EL1.SS (bit 0) via ARM_DEBUG_STATE64 enables the software-step
//     state machine for the thread;
//   - PSTATE.SS (CPSR bit 21) via ARM_THREAD_STATE64 arms "active-not-pending"
//     so exactly one instruction executes before the step exception fires.
// With every other thread Mach-suspended and this bit set on the target, a
// per-process PT_STEP steps precisely the intended thread.
static inline kern_return_t bingo_set_single_step(mach_port_t thread, int on) {
    arm_debug_state64_t dbg;
    mach_msg_type_number_t dcount = ARM_DEBUG_STATE64_COUNT;
    kern_return_t kr = thread_get_state(
        thread, ARM_DEBUG_STATE64, (thread_state_t)&dbg, &dcount);
    if (kr != KERN_SUCCESS) return kr;
    if (on) dbg.__mdscr_el1 |= 1ULL;
    else    dbg.__mdscr_el1 &= ~1ULL;
    kr = thread_set_state(
        thread, ARM_DEBUG_STATE64, (thread_state_t)&dbg, ARM_DEBUG_STATE64_COUNT);
    if (kr != KERN_SUCCESS) return kr;

    arm_thread_state64_t st;
    mach_msg_type_number_t tcount = ARM_THREAD_STATE64_COUNT;
    kr = thread_get_state(
        thread, ARM_THREAD_STATE64, (thread_state_t)&st, &tcount);
    if (kr != KERN_SUCCESS) return kr;
    if (on) st.__cpsr |= (1U << 21);
    else    st.__cpsr &= ~(1U << 21);
    return thread_set_state(
        thread, ARM_THREAD_STATE64, (thread_state_t)&st, ARM_THREAD_STATE64_COUNT);
}