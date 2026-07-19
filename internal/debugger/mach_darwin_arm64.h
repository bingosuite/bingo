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
// Icache coherency on Apple Silicon (ARM64) — THE fix for the step-over/step-out
// wedge (issue #92):
// mach_vm_write updates the target's data view of the page, and the
// task_suspend/task_resume pair below drains the in-flight instruction pipeline.
// Neither, however, INVALIDATES the target CPU's L1 instruction cache. When a
// freshly-patched breakpoint address is re-executed within microseconds of the
// write (e.g. a <stepout-return> trap that fires the instant a callee returns,
// or a <stepover-next> trap on a hot loop line), the core can still fetch the
// STALE cached original instruction and run straight past the trap — the stop is
// silently missed and the debugger wedges waiting for an event that never comes.
// It is intermittent (~2.5% on step-out) precisely because it only bites when
// the line is still resident in the I-cache at re-execution; a trap re-hit a
// full loop iteration later never flakes because the line has since been
// evicted. An artificial delay before the resume masked it, which is the
// classic self-modifying-code cache-coherency signature.
//
// mach_vm_machine_attribute(MATTR_CACHE, MATTR_VAL_CACHE_FLUSH) DOES work on
// Apple Silicon (returns KERN_SUCCESS — an earlier comment here claimed
// KERN_NOT_SUPPORTED; that was wrong, verified empirically on M-series). It
// cleans the data cache and invalidates the instruction cache for the patched
// range in the TARGET task, which is exactly the cross-task SMC synchronization
// the CPU needs. We issue it on every write; a failure is non-fatal (best
// effort) but must not mask the underlying write's own error.
//
// Suspend-count accounting: Mach maintains a SINGLE task-level suspend_count
// per task. The suspend/resume here are strictly balanced (every exit path
// below calls task_resume) and this function only ever runs on the engine's
// single locked OS thread (see AGENTS.md — all ptrace/Mach calls are serialized
// there), so the pair can never interleave with itself and the count returns to
// its baseline before we return.
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

    // Invalidate the target's instruction cache for the patched range so a
    // near-immediate re-execution of the address sees the new bytes rather than
    // a stale I-cache line (see the SMC coherency note above). Best effort: the
    // write itself already succeeded, so a flush failure must not clobber kr.
    int flush = MATTR_VAL_CACHE_FLUSH;
    mach_vm_machine_attribute(task, addr, n, MATTR_CACHE, &flush);

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
// The caller must vm_deallocate the returned threads array and
// mach_port_deallocate every returned thread port (each is a send right).
static inline kern_return_t bingo_thread_list(
    mach_port_t task,
    thread_act_port_array_t *threads_out,
    mach_msg_type_number_t *count_out)
{
    return task_threads(task, threads_out, count_out);
}

// bingo_port_deallocate releases one user reference to a Mach port name in our
// own IPC space (used to release thread send-rights returned by task_threads).
static inline kern_return_t bingo_port_deallocate(mach_port_t port) {
    return mach_port_deallocate(mach_task_self(), port);
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

// --- Exception-port launch + mach_msg receive loop (issue #92) ---------------
// Design: pure Mach, no ptrace/PT_SIGEXC. Launch suspended via posix_spawn, then
// register a TASK-level EXC_MASK_BREAKPOINT exception port and detect every stop
// (software BRK #0 and ARMv8 hardware single-step both raise EXC_BREAKPOINT) by
// receiving on a port set. Only EXC_MASK_BREAKPOINT is masked: BSD signals are
// deliberately left to native, thread-directed delivery so the Go runtime's
// thread-directed SIGURG reaches the exact M it targeted — this is what lets us
// re-enable async preemption in the tracee (the #92 fix). See AGENTS.md.

#include <spawn.h>
#include <mach/notify.h>
#include <mach/mig_errors.h>

extern char **environ;

// bingo_posix_spawn launches path with POSIX_SPAWN_START_SUSPENDED: the child is
// created and its image mapped, but left Mach-suspended at its entry point
// (before dyld runs any user code) so we win the race to attach the exception
// port. fds and cwd are inherited from the parent (matching the previous
// exec.Command default). Returns 0 on success (pid in *pid_out) or the errno
// posix_spawn reports.
static inline int bingo_posix_spawn(
    const char *path, char *const argv[], char *const envp[], int *pid_out)
{
    posix_spawnattr_t attr;
    if (posix_spawnattr_init(&attr) != 0) return -1;
    posix_spawnattr_setflags(&attr, POSIX_SPAWN_START_SUSPENDED);
    pid_t pid = 0;
    int rc = posix_spawn(&pid, path, NULL, &attr, argv,
                         envp ? envp : environ);
    posix_spawnattr_destroy(&attr);
    if (rc != 0) return rc;
    *pid_out = (int)pid;
    return 0;
}

// bingo_setup_exception_ports registers a task-level EXC_MASK_BREAKPOINT
// exception port, a dead-name notification port (fires when the tracee exits),
// and a control port used to wake a blocked receive for Pause — all moved into
// one port set the receive loop waits on. THREAD_STATE_NONE keeps the exception
// message small (no register state inline); the engine reads registers via
// thread_get_state when it needs them.
static inline kern_return_t bingo_setup_exception_ports(
    task_t task, mach_port_t *port_set, mach_port_t *exc_port,
    mach_port_t *note_port, mach_port_t *ctrl_port)
{
    kern_return_t kr;
    mach_port_t self = mach_task_self();
    mach_port_t prev_not;

    kr = mach_port_allocate(self, MACH_PORT_RIGHT_RECEIVE, exc_port);
    if (kr != KERN_SUCCESS) return kr;
    kr = mach_port_insert_right(self, *exc_port, *exc_port, MACH_MSG_TYPE_MAKE_SEND);
    if (kr != KERN_SUCCESS) return kr;
    kr = task_set_exception_ports(task, EXC_MASK_BREAKPOINT, *exc_port,
            EXCEPTION_DEFAULT, THREAD_STATE_NONE);
    if (kr != KERN_SUCCESS) return kr;

    kr = mach_port_allocate(self, MACH_PORT_RIGHT_RECEIVE, note_port);
    if (kr != KERN_SUCCESS) return kr;
    kr = mach_port_insert_right(self, *note_port, *note_port, MACH_MSG_TYPE_MAKE_SEND);
    if (kr != KERN_SUCCESS) return kr;
    kr = mach_port_request_notification(self, task, MACH_NOTIFY_DEAD_NAME, 0,
            *note_port, MACH_MSG_TYPE_MAKE_SEND_ONCE, &prev_not);
    if (kr != KERN_SUCCESS) return kr;

    kr = mach_port_allocate(self, MACH_PORT_RIGHT_RECEIVE, ctrl_port);
    if (kr != KERN_SUCCESS) return kr;
    kr = mach_port_insert_right(self, *ctrl_port, *ctrl_port, MACH_MSG_TYPE_MAKE_SEND);
    if (kr != KERN_SUCCESS) return kr;

    kr = mach_port_allocate(self, MACH_PORT_RIGHT_PORT_SET, port_set);
    if (kr != KERN_SUCCESS) return kr;
    kr = mach_port_move_member(self, *exc_port, *port_set);
    if (kr != KERN_SUCCESS) return kr;
    kr = mach_port_move_member(self, *note_port, *port_set);
    if (kr != KERN_SUCCESS) return kr;
    return mach_port_move_member(self, *ctrl_port, *port_set);
}

// bingo_freeze_at_launch converts the POSIX_SPAWN_START_SUSPENDED task-level
// suspension into bingo's resting state: every thread individually Mach-suspended
// (suspend_count 1) with the task itself resumed. After this nothing runs but the
// process is ready for per-thread single-step / continue.
static inline kern_return_t bingo_freeze_at_launch(task_t task) {
    thread_act_array_t list;
    mach_msg_type_number_t n, i;
    kern_return_t kr = task_threads(task, &list, &n);
    if (kr != KERN_SUCCESS) return kr;
    for (i = 0; i < n; i++) {
        thread_suspend(list[i]);
        mach_port_deallocate(mach_task_self(), list[i]);
    }
    vm_deallocate(mach_task_self(), (vm_address_t)list, n * sizeof(list[0]));
    return task_resume(task);
}

// bingo_num_running_threads returns how many threads have suspend_count == 0.
static inline int bingo_num_running_threads(task_t task) {
    thread_act_array_t list;
    mach_msg_type_number_t n, i;
    int running = 0;
    if (task_threads(task, &list, &n) != KERN_SUCCESS) return -1;
    for (i = 0; i < n; i++) {
        struct thread_basic_info info;
        mach_msg_type_number_t c = THREAD_BASIC_INFO_COUNT;
        if (thread_info(list[i], THREAD_BASIC_INFO, (thread_info_t)&info, &c) == KERN_SUCCESS) {
            if (info.suspend_count == 0) running++;
        }
        mach_port_deallocate(mach_task_self(), list[i]);
    }
    vm_deallocate(mach_task_self(), (vm_address_t)list, n * sizeof(list[0]));
    return running;
}

// bingo_resume_all_threads normalizing-resumes every thread to suspend_count 0
// (the "continue the world" primitive). thread_resume is issued suspend_count
// times so a thread suspended more than once (faulted, then stop-the-world'd) is
// fully released. Mirrors Delve's resume_thread applied across the task.
static inline kern_return_t bingo_resume_all_threads(task_t task) {
    thread_act_array_t list;
    mach_msg_type_number_t n, i;
    kern_return_t kr = task_threads(task, &list, &n);
    if (kr != KERN_SUCCESS) return kr;
    for (i = 0; i < n; i++) {
        struct thread_basic_info info;
        mach_msg_type_number_t c = THREAD_BASIC_INFO_COUNT;
        if (thread_info(list[i], THREAD_BASIC_INFO, (thread_info_t)&info, &c) == KERN_SUCCESS) {
            for (int k = 0; k < info.suspend_count; k++) thread_resume(list[i]);
        }
        mach_port_deallocate(mach_task_self(), list[i]);
    }
    vm_deallocate(mach_task_self(), (vm_address_t)list, n * sizeof(list[0]));
    return KERN_SUCCESS;
}

// bingo_resume_one_thread normalizing-resumes a single thread to suspend_count 0
// (used to run exactly one thread for a single-step while the rest stay held).
static inline kern_return_t bingo_resume_one_thread(mach_port_t thread) {
    struct thread_basic_info info;
    mach_msg_type_number_t c = THREAD_BASIC_INFO_COUNT;
    kern_return_t kr = thread_info(thread, THREAD_BASIC_INFO, (thread_info_t)&info, &c);
    if (kr != KERN_SUCCESS) return kr;
    for (int k = 0; k < info.suspend_count; k++) {
        kr = thread_resume(thread);
        if (kr != KERN_SUCCESS) return kr;
    }
    return KERN_SUCCESS;
}

// bingo__send_reply acknowledges an exception message with KERN_SUCCESS so the
// kernel considers it handled. The faulting thread is thread_suspend'd BEFORE the
// reply (by the caller), so "handled" does not actually resume it — that is how a
// stop is held. Reply id is request id + 100 (MIG convention).
static inline kern_return_t bingo__send_reply(mach_msg_header_t *hdr) {
    mig_reply_error_t reply;
    mach_msg_header_t *rh = &reply.Head;
    rh->msgh_bits = MACH_MSGH_BITS(MACH_MSGH_BITS_REMOTE(hdr->msgh_bits), 0);
    rh->msgh_remote_port = hdr->msgh_remote_port;
    rh->msgh_size = (mach_msg_size_t)sizeof(mig_reply_error_t);
    rh->msgh_local_port = MACH_PORT_NULL;
    rh->msgh_id = hdr->msgh_id + 100;
    reply.NDR = NDR_record;
    reply.RetCode = KERN_SUCCESS;
    return mach_msg(&reply.Head, MACH_SEND_MSG | MACH_SEND_INTERRUPT,
        rh->msgh_size, 0, MACH_PORT_NULL, MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);
}

// Message classes returned by bingo_mach_recv / bingo_stop_the_world.
#define BINGO_MSG_NONE   0  // timeout or interrupted; caller retries
#define BINGO_MSG_EXC    1  // exception (thread halted at BRK / single-step trap)
#define BINGO_MSG_DEATH  2  // dead-name notification: the tracee exited
#define BINGO_MSG_PAUSE  3  // control-port wake (Pause requested)
#define BINGO_MSG_ERROR  (-1)

// Sentinel msgh_id for the Pause wake sent to the control port. Chosen well clear
// of the exception (2401) and dead-name (72) ids.
#define BINGO_CTRL_MSG_ID 0x42420

// bingo_reply_exception acknowledges a previously-received exception with
// KERN_SUCCESS, using the reply header fields captured by bingo_mach_recv. It is
// the DEFERRED counterpart to the inline reply: bingo replies to the faulting
// thread's exception only when it is about to resume that thread, NOT when the
// exception is received. This matters because bingo leaves BSD signals native
// (only EXC_MASK_BREAKPOINT is masked): if we reply immediately, the kernel
// returns the still-suspended thread toward user space and, seeing a pending
// signal (e.g. Go async-preemption SIGURG), builds a signal-handler frame —
// moving the thread's PC into _sigtramp before the engine can read it, so the
// stop is misread. An UN-acknowledged Mach exception keeps the thread frozen at
// the faulting instruction with a stable PC; replying at resume time delivers
// the pending signal correctly, on the exact thread the kernel targeted. remote
// carries MACH_MSGH_BITS_REMOTE(msgh_bits); id is the original request id.
static inline kern_return_t bingo_reply_exception(
    mach_port_t remote_port, unsigned int remote_bits, int id)
{
    mig_reply_error_t reply;
    mach_msg_header_t *rh = &reply.Head;
    rh->msgh_bits = MACH_MSGH_BITS(remote_bits, 0);
    rh->msgh_remote_port = remote_port;
    rh->msgh_size = (mach_msg_size_t)sizeof(mig_reply_error_t);
    rh->msgh_local_port = MACH_PORT_NULL;
    rh->msgh_id = id + 100;
    reply.NDR = NDR_record;
    reply.RetCode = KERN_SUCCESS;
    return mach_msg(&reply.Head, MACH_SEND_MSG | MACH_SEND_INTERRUPT,
        rh->msgh_size, 0, MACH_PORT_NULL, MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);
}

// bingo_mach_recv receives one message from the port set. timeout_ms < 0 blocks
// indefinitely; >= 0 waits that long. On an exception it extracts the faulting
// thread port (thread_out) and the exception type / first code, thread_suspend's
// the faulting thread when do_suspend is set, and either replies immediately
// (do_reply != 0) or hands the reply header back via reply_* out-params for a
// deferred bingo_reply_exception (do_reply == 0 — the default for the main
// receive loop; see bingo_reply_exception for why deferral is required).
// Returns one of the BINGO_MSG_* classes.
static inline int bingo_mach_recv(
    mach_port_t port_set, int timeout_ms,
    mach_port_t *thread_out, int *exc_out, int64_t *code0_out, int *id_out,
    int do_suspend, int do_reply,
    mach_port_t *reply_port_out, unsigned int *reply_bits_out, int *reply_id_out)
{
    union { mach_msg_header_t hdr; char buf[512]; } msg;
    mach_msg_option_t opts = MACH_RCV_MSG | MACH_RCV_INTERRUPT;
    mach_msg_timeout_t to = MACH_MSG_TIMEOUT_NONE;
    if (timeout_ms >= 0) { opts |= MACH_RCV_TIMEOUT; to = (mach_msg_timeout_t)timeout_ms; }

    kern_return_t kr = mach_msg(&msg.hdr, opts, 0, sizeof(msg.buf),
                                port_set, to, MACH_PORT_NULL);
    if (kr == MACH_RCV_TIMED_OUT || kr == MACH_RCV_INTERRUPTED) return BINGO_MSG_NONE;
    if (kr != MACH_MSG_SUCCESS) return BINGO_MSG_ERROR;

    if (id_out) *id_out = (int)msg.hdr.msgh_id;

    switch (msg.hdr.msgh_id) {
    case 2401: { // exception_raise (see xnu osfmk/mach/exc.defs)
        mach_msg_body_t *bod = (mach_msg_body_t *)(&msg.hdr + 1);
        mach_msg_port_descriptor_t *desc = (mach_msg_port_descriptor_t *)(bod + 1);
        mach_port_t thread = desc[0].name; // desc[1].name == task
        NDR_record_t *ndr = (NDR_record_t *)(desc + 2);
        integer_t *data = (integer_t *)(ndr + 1);
        // data[0]=exception, data[1]=codeCnt, data[2]=code[0], data[3]=code[1].
        if (thread_out) *thread_out = thread;
        if (exc_out) *exc_out = (int)data[0];
        if (code0_out) *code0_out = (int64_t)data[2];
        // The kernel inserts a send right to BOTH the faulting thread and the
        // task into our IPC space for every exception message; Mach coalesces
        // them to our existing names and bumps each name's user-ref count. We
        // never use the task descriptor (we hold a cached task port), so release
        // it here — otherwise a single name's uref grows by one per stop and
        // eventually hits KERN_UREFS_OVERFLOW, after which the kernel can no
        // longer copy out the exception and Wait wedges. The thread right IS
        // handed back via thread_out; its accounting (adopt-or-release into the
        // retained per-thread set) is the caller's responsibility.
        mach_port_deallocate(mach_task_self(), desc[1].name);
        if (do_suspend) thread_suspend(thread);
        if (do_reply) {
            if (bingo__send_reply(&msg.hdr) != MACH_MSG_SUCCESS) return BINGO_MSG_ERROR;
        } else {
            if (reply_port_out) *reply_port_out = msg.hdr.msgh_remote_port;
            if (reply_bits_out) *reply_bits_out = MACH_MSGH_BITS_REMOTE(msg.hdr.msgh_bits);
            if (reply_id_out) *reply_id_out = (int)msg.hdr.msgh_id;
        }
        return BINGO_MSG_EXC;
    }
    case 72: // MACH_NOTIFY_DEAD_NAME
        return BINGO_MSG_DEATH;
    case BINGO_CTRL_MSG_ID:
        return BINGO_MSG_PAUSE;
    default:
        return BINGO_MSG_NONE;
    }
}

// bingo_stop_the_world brings the task to bingo's resting state: every thread
// Mach-suspended with NO exception left queued. It suspends each running thread,
// then DRAINS any breakpoint exceptions that were already in flight (suspending +
// replying each), because thread_suspend does not flush a thread's already-queued
// Mach exception — leaving one queued lets it resurface mid-single-step and be
// misread as the step's completion (the concurrent-fault hang). Loops until no
// thread is running. Returns BINGO_MSG_NONE when stopped, BINGO_MSG_DEATH if the
// tracee exited during the stop, or BINGO_MSG_ERROR.
static inline int bingo_stop_the_world(task_t task, mach_port_t port_set) {
    for (int iter = 0; iter < 128; iter++) {
        thread_act_array_t list;
        mach_msg_type_number_t n, i;
        if (task_threads(task, &list, &n) != KERN_SUCCESS) return BINGO_MSG_ERROR;
        for (i = 0; i < n; i++) {
            struct thread_basic_info info;
            mach_msg_type_number_t c = THREAD_BASIC_INFO_COUNT;
            if (thread_info(list[i], THREAD_BASIC_INFO, (thread_info_t)&info, &c) == KERN_SUCCESS) {
                if (info.suspend_count == 0) thread_suspend(list[i]);
            }
            mach_port_deallocate(mach_task_self(), list[i]);
        }
        vm_deallocate(mach_task_self(), (vm_address_t)list, n * sizeof(list[0]));
        for (;;) {
            mach_port_t th = 0; int exc = 0, id = 0; int64_t c0 = 0;
            int cls = bingo_mach_recv(port_set, 0, &th, &exc, &c0, &id, 1, 1, 0, 0, 0);
            if (cls == BINGO_MSG_DEATH) return BINGO_MSG_DEATH;
            if (cls == BINGO_MSG_EXC) {
                // Absorbed a queued fault: it was replied immediately (do_reply=1)
                // and its thread right is not handed to Go, so release it here.
                // The thread stays suspended and re-faults on the next resume; the
                // task right was already released inside bingo_mach_recv. Without
                // this each drained straggler leaks one per-thread send right.
                mach_port_deallocate(mach_task_self(), th);
                continue;
            }
            break; // none / pause / error
        }
        int running = bingo_num_running_threads(task);
        if (running == 0) return BINGO_MSG_NONE;
        if (running < 0) return BINGO_MSG_ERROR;
    }
    return BINGO_MSG_ERROR;
}

// bingo_send_ctrl wakes a blocked bingo_mach_recv for Pause by sending an empty
// sentinel message to the control port. Non-blocking (MACH_SEND_TIMEOUT 0): if a
// prior wake is still queued the send is dropped, which is fine — one pending
// Pause is enough.
static inline kern_return_t bingo_send_ctrl(mach_port_t ctrl_port) {
    mach_msg_header_t h;
    h.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, 0);
    h.msgh_size = sizeof(h);
    h.msgh_remote_port = ctrl_port;
    h.msgh_local_port = MACH_PORT_NULL;
    h.msgh_voucher_port = MACH_PORT_NULL;
    h.msgh_id = BINGO_CTRL_MSG_ID;
    return mach_msg(&h, MACH_SEND_MSG | MACH_SEND_TIMEOUT, sizeof(h), 0,
                    MACH_PORT_NULL, 0, MACH_PORT_NULL);
}

// --- Diagnostic-only helpers (gated behind BINGO_DARWIN_SUSPEND_PROBE) --------
// These are PURE READS (task_info / thread_info / thread_get_state) and do NOT
// perturb any suspend count, so they are safe to call against a wedged/hung
// task. They exist to answer, empirically, whether a residual step-over wedge
// coincides with a leaked TASK-level Mach suspend count.

// bingo_task_suspend_count reads the task-level Mach suspend_count via
// task_info(MACH_TASK_BASIC_INFO). A running (un-suspended) task reports 0;
// any value >= 1 means the task is held suspended (nothing will run, so a
// wait4 for the next ptrace stop can never return).
static inline kern_return_t bingo_task_suspend_count(
    mach_port_t task, uint32_t *out)
{
    mach_task_basic_info_data_t info;
    mach_msg_type_number_t count = MACH_TASK_BASIC_INFO_COUNT;
    kern_return_t kr = task_info(
        task, MACH_TASK_BASIC_INFO, (task_info_t)&info, &count);
    if (kr != KERN_SUCCESS) return kr;
    *out = (uint32_t)info.suspend_count;
    return KERN_SUCCESS;
}

// bingo_thread_probe reads run_state, per-thread suspend_count and PC for one
// thread. run_state: 1=RUNNING 2=STOPPED 3=WAITING 4=UNINTERRUPTIBLE 5=HALTED.
static inline kern_return_t bingo_thread_probe(
    mach_port_t thread, int *run_state, int *suspend_count, uint64_t *pc)
{
    struct thread_basic_info tbi;
    mach_msg_type_number_t bcount = THREAD_BASIC_INFO_COUNT;
    kern_return_t kr = thread_info(
        thread, THREAD_BASIC_INFO, (thread_info_t)&tbi, &bcount);
    if (kr != KERN_SUCCESS) return kr;
    *run_state = (int)tbi.run_state;
    *suspend_count = (int)tbi.suspend_count;

    arm_thread_state64_t st;
    mach_msg_type_number_t tcount = ARM_THREAD_STATE64_COUNT;
    kr = thread_get_state(
        thread, ARM_THREAD_STATE64, (thread_state_t)&st, &tcount);
    *pc = (kr == KERN_SUCCESS) ? (uint64_t)st.__pc : 0;
    return KERN_SUCCESS;
}
// bingo_port_send_refs returns the number of send-right user references held on
// a port NAME in our own IPC space, or -1 on error. Pure read (mach_port_get_refs
// on mach_task_self). Used by the darwin port-hygiene regression test to assert
// the exception-receive path does not leak a task/thread send right per stop — a
// per-stop leak grows one name's uref unboundedly toward KERN_UREFS_OVERFLOW, at
// which point the kernel can no longer copy out the exception message and Wait
// wedges.
static inline int bingo_port_send_refs(mach_port_t name) {
    mach_port_urefs_t refs = 0;
    if (mach_port_get_refs(mach_task_self(), name, MACH_PORT_RIGHT_SEND, &refs) != KERN_SUCCESS)
        return -1;
    return (int)refs;
}
