#include <errno.h>
#include <inttypes.h>
#include <mach/arm/thread_status.h>
#include <mach/exc.h>
#include <mach/mach.h>
#include <pthread.h>
#include <signal.h>
#include <stdarg.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ucontext.h>
#include <time.h>
#include <unistd.h>

#if !defined(__APPLE__) || !defined(__arm64__)
#error "This fixture requires darwin/arm64."
#endif

_Static_assert(ATOMIC_LLONG_LOCK_FREE == 2,
               "The SIGTRAP counter must not acquire a signal-unsafe lock.");

enum { worker_count = 4, watchdog_seconds = 45 };

static atomic_ullong heartbeat;
static atomic_ullong worker_progress;
static atomic_ullong custom_traps;
static atomic_ullong native_traps;
static atomic_bool stop_workers;
static atomic_bool stop_handler;
static const char *gate_path;
static mach_port_t handler_port = MACH_PORT_NULL;

extern const uint32_t bingo_bp_site;

struct handler_snapshot {
    exception_mask_t mask;
    mach_port_t port;
    exception_behavior_t behavior;
    thread_state_flavor_t flavor;
    mach_msg_type_number_t count;
};

__attribute__((noreturn, format(printf, 1, 2)))
static void fail(const char *format, ...) {
    va_list args;
    va_start(args, format);
    fputs("darwin_attach_target: ", stderr);
    vfprintf(stderr, format, args);
    fputc('\n', stderr);
    va_end(args);
    exit(2);
}

static void check_mach(kern_return_t result, const char *operation) {
    if (result != KERN_SUCCESS)
        fail("%s: %s (%d)", operation, mach_error_string(result), result);
}

static void check_pthread(int result, const char *operation) {
    if (result != 0)
        fail("%s: %s (%d)", operation, strerror(result), result);
}

static bool is_brk(uintptr_t pc) {
    return pc != 0 && (pc & 3) == 0 &&
           (*(const volatile uint32_t *)pc & UINT32_C(0xffe0001f)) ==
               UINT32_C(0xd4200000);
}

__attribute__((noinline))
static void bingo_worker_site(void) {
    /* The label and DWARF row must name the same word the debugger restores. */
    __asm__ volatile(".globl _bingo_bp_site\n_bingo_bp_site:\n\tnop" ::: "memory"); // BINGO_BP
    atomic_fetch_add_explicit(&worker_progress, 1, memory_order_relaxed);
}

static void *worker(void *unused) {
    (void)unused;
    while (!atomic_load_explicit(&stop_workers, memory_order_relaxed)) {
        atomic_fetch_add_explicit(&heartbeat, 1, memory_order_relaxed);
        if (access(gate_path, F_OK) == 0) {
            bingo_worker_site();
        } else if (errno != ENOENT) {
            fail("access gate %s: %s", gate_path, strerror(errno));
        }
        struct timespec delay = {.tv_nsec = 1000000};
        while (nanosleep(&delay, &delay) != 0) {
            if (errno != EINTR)
                fail("nanosleep: %s", strerror(errno));
        }
    }
    return NULL;
}

static void native_trap(int signal_number, siginfo_t *info, void *context) {
    (void)info;
    ucontext_t *uc = context;
    if (signal_number != SIGTRAP || uc == NULL || uc->uc_mcontext == NULL ||
        !is_brk(arm_thread_state64_get_pc(uc->uc_mcontext->__ss))) {
        static const char message[] =
            "darwin_attach_target: SIGTRAP did not stop at an ARM64 BRK\n";
        (void)write(STDERR_FILENO, message, sizeof(message) - 1);
        _exit(3);
    }
    uintptr_t pc = arm_thread_state64_get_pc(uc->uc_mcontext->__ss);
    arm_thread_state64_set_pc_fptr(uc->uc_mcontext->__ss, (void *)(pc + 4));
    atomic_fetch_add_explicit(&native_traps, 1, memory_order_relaxed);
}

static void *exception_server(void *unused) {
    (void)unused;
    while (!atomic_load_explicit(&stop_handler, memory_order_relaxed)) {
        struct {
            __Request__exception_raise_t request;
            mach_msg_max_trailer_t trailer;
        } message = {0};
        mach_msg_header_t *head = &message.request.Head;
        kern_return_t result =
            mach_msg(head, MACH_RCV_MSG | MACH_RCV_TIMEOUT, 0, sizeof(message),
                     handler_port, 100, MACH_PORT_NULL);
        if (result == MACH_RCV_TIMED_OUT)
            continue;
        check_mach(result, "mach_msg receive exception");

        __Request__exception_raise_t *request = &message.request;
        if (head->msgh_id != 2401 ||
            head->msgh_size != sizeof(*request) ||
            !(head->msgh_bits & MACH_MSGH_BITS_COMPLEX) ||
            request->msgh_body.msgh_descriptor_count != 2 ||
            request->thread.type != MACH_MSG_PORT_DESCRIPTOR ||
            request->task.type != MACH_MSG_PORT_DESCRIPTOR ||
            request->thread.disposition != MACH_MSG_TYPE_PORT_SEND ||
            request->task.disposition != MACH_MSG_TYPE_PORT_SEND ||
            request->exception != EXC_BREAKPOINT || request->codeCnt != 2 ||
            request->task.name != mach_task_self()) {
            mach_msg_destroy(head);
            fail("unexpected Mach exception message");
        }

        arm_thread_state64_t state = {0};
        mach_msg_type_number_t count = ARM_THREAD_STATE64_COUNT;
        check_mach(thread_get_state(request->thread.name, ARM_THREAD_STATE64,
                                    (thread_state_t)&state, &count),
                   "thread_get_state for BRK");
        uintptr_t pc = arm_thread_state64_get_pc(state);
        if (count != ARM_THREAD_STATE64_COUNT || !is_brk(pc))
            fail("Mach breakpoint did not stop at an ARM64 BRK (pc=%" PRIxPTR ")",
                 pc);
        arm_thread_state64_set_pc_fptr(state, (void *)(pc + 4));
        check_mach(thread_set_state(request->thread.name, ARM_THREAD_STATE64,
                                    (thread_state_t)&state, count),
                   "thread_set_state after BRK");
        atomic_fetch_add_explicit(&custom_traps, 1, memory_order_relaxed);

        __Reply__exception_raise_t reply = {0};
        reply.Head.msgh_bits =
            MACH_MSGH_BITS(MACH_MSGH_BITS_REMOTE(head->msgh_bits), 0);
        reply.Head.msgh_size = sizeof(reply);
        reply.Head.msgh_remote_port = head->msgh_remote_port;
        reply.Head.msgh_id = head->msgh_id + 100;
        reply.NDR = NDR_record;
        reply.RetCode = KERN_SUCCESS;

        /* The kernel copied a send right for each descriptor, even for self. */
        check_mach(mach_port_deallocate(mach_task_self(), request->thread.name),
                   "deallocate exception thread right");
        check_mach(mach_port_deallocate(mach_task_self(), request->task.name),
                   "deallocate exception task right");
        check_mach(mach_msg(&reply.Head, MACH_SEND_MSG | MACH_SEND_TIMEOUT,
                            sizeof(reply), 0, MACH_PORT_NULL, 1000,
                            MACH_PORT_NULL),
                   "mach_msg reply to BRK");
    }
    return NULL;
}

static struct handler_snapshot read_handler(void) {
    exception_mask_t masks[EXC_TYPES_COUNT] = {0};
    mach_port_t ports[EXC_TYPES_COUNT] = {0};
    exception_behavior_t behaviors[EXC_TYPES_COUNT] = {0};
    thread_state_flavor_t flavors[EXC_TYPES_COUNT] = {0};
    mach_msg_type_number_t count = EXC_TYPES_COUNT;
    check_mach(task_get_exception_ports(mach_task_self(), EXC_MASK_BREAKPOINT,
                                        masks, &count, ports, behaviors, flavors),
               "task_get_exception_ports");
    struct handler_snapshot snapshot = {.count = count};
    if (count != 0) {
        snapshot.mask = masks[0];
        snapshot.port = ports[0];
        snapshot.behavior = behaviors[0];
        snapshot.flavor = flavors[0];
    }
    for (mach_msg_type_number_t i = 0; i < count; ++i) {
        if (MACH_PORT_VALID(ports[i]))
            check_mach(mach_port_deallocate(mach_task_self(), ports[i]),
                       "deallocate task_get_exception_ports right");
    }
    if (count > 1)
        fail("one-bit exception mask returned %u handler tuples", count);
    return snapshot;
}

static void report(const char *event) {
    struct handler_snapshot snapshot = read_handler();
    uint32_t instruction = *(const volatile uint32_t *)&bingo_bp_site;
    if (printf("{\"event\":\"%s\",\"pid\":%d,\"mask\":%u,\"port\":%u,"
               "\"behavior\":%d,\"flavor\":%d,\"count\":%u,"
               "\"heartbeat\":%llu,\"worker_progress\":%llu,"
               "\"custom_traps\":%llu,\"native_traps\":%llu,"
               "\"instruction\":%" PRIu32 "}\n",
               event, getpid(), snapshot.mask, snapshot.port, snapshot.behavior,
               snapshot.flavor, snapshot.count,
               atomic_load_explicit(&heartbeat, memory_order_relaxed),
               atomic_load_explicit(&worker_progress, memory_order_relaxed),
               atomic_load_explicit(&custom_traps, memory_order_relaxed),
               atomic_load_explicit(&native_traps, memory_order_relaxed),
               instruction) < 0 ||
        fflush(stdout) != 0)
        fail("write JSON status: %s", strerror(errno));
}

int main(int argc, char **argv) {
    if (argc != 3 ||
        (strcmp(argv[1], "custom") != 0 && strcmp(argv[1], "none") != 0))
        fail("usage: %s custom|none gate-path", argv[0]);
    bool custom = strcmp(argv[1], "custom") == 0;
    gate_path = argv[2];

    struct sigaction action = {.sa_handler = SIG_DFL};
    if (sigemptyset(&action.sa_mask) != 0 ||
        sigaction(SIGALRM, &action, NULL) != 0)
        fail("install watchdog: %s", strerror(errno));
    sigset_t unblocked;
    if (sigemptyset(&unblocked) != 0 || sigaddset(&unblocked, SIGALRM) != 0 ||
        sigaddset(&unblocked, SIGTRAP) != 0 ||
        sigprocmask(SIG_UNBLOCK, &unblocked, NULL) != 0)
        fail("unblock watchdog and trap signals: %s", strerror(errno));
    alarm(watchdog_seconds);
    action.sa_handler = SIG_IGN;
    if (sigaction(SIGPIPE, &action, NULL) != 0)
        fail("ignore SIGPIPE for checked stdout writes: %s", strerror(errno));

    pthread_t server_thread;
    if (custom) {
        check_mach(mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE,
                                      &handler_port),
                   "allocate exception receive right");
        /* Retain both rights until exit so snapshots keep the same port name. */
        check_mach(mach_port_insert_right(mach_task_self(), handler_port,
                                          handler_port, MACH_MSG_TYPE_MAKE_SEND),
                   "insert exception send right");
        check_pthread(pthread_create(&server_thread, NULL, exception_server, NULL),
                      "create exception server");
        check_mach(task_set_exception_ports(mach_task_self(), EXC_MASK_BREAKPOINT,
                                            handler_port, EXCEPTION_DEFAULT,
                                            ARM_THREAD_STATE64),
                   "install custom breakpoint handler");
    } else {
        action.sa_sigaction = native_trap;
        action.sa_flags = SA_SIGINFO;
        if (sigaction(SIGTRAP, &action, NULL) != 0)
            fail("install native SIGTRAP handler: %s", strerror(errno));
    }

    struct handler_snapshot baseline = read_handler();
    if (custom) {
        if (baseline.count != 1 || baseline.mask != EXC_MASK_BREAKPOINT ||
            baseline.port != handler_port ||
            baseline.behavior != EXCEPTION_DEFAULT ||
            baseline.flavor != ARM_THREAD_STATE64)
            fail("custom breakpoint handler tuple was not installed exactly");
    } else if (baseline.port != MACH_PORT_NULL) {
        fail("none mode inherited a task breakpoint handler (port=%u)",
             baseline.port);
    }
    if (bingo_bp_site != UINT32_C(0xd503201f))
        fail("source breakpoint site is not the original ARM64 NOP");

    pthread_t workers[worker_count];
    for (int i = 0; i < worker_count; ++i)
        check_pthread(pthread_create(&workers[i], NULL, worker, NULL),
                      "create worker");
    report("ready");

    char command[64];
    for (;;) {
        if (fgets(command, sizeof(command), stdin) == NULL) {
            if (ferror(stdin))
                fail("read command: %s", strerror(errno));
            fail("stdin closed before exit command");
        }
        command[strcspn(command, "\r\n")] = '\0';
        if (strcmp(command, "status") == 0) {
            report("status");
        } else if (strcmp(command, "trap") == 0) {
            __asm__ volatile("brk #0" ::: "memory");
            report("trap");
        } else if (strcmp(command, "exit") == 0) {
            break;
        } else {
            fail("unknown command: %s", command);
        }
    }

    atomic_store_explicit(&stop_workers, true, memory_order_relaxed);
    for (int i = 0; i < worker_count; ++i)
        check_pthread(pthread_join(workers[i], NULL), "join worker");
    if (custom) {
        /* A worker can still owe an exception reply until its join completes. */
        atomic_store_explicit(&stop_handler, true, memory_order_relaxed);
        check_pthread(pthread_join(server_thread, NULL), "join exception server");
        check_mach(task_set_exception_ports(mach_task_self(), EXC_MASK_BREAKPOINT,
                                            MACH_PORT_NULL, EXCEPTION_DEFAULT,
                                            THREAD_STATE_NONE),
                   "remove custom breakpoint handler");
        check_mach(mach_port_deallocate(mach_task_self(), handler_port),
                   "deallocate exception send right");
        check_mach(mach_port_mod_refs(mach_task_self(), handler_port,
                                      MACH_PORT_RIGHT_RECEIVE, -1),
                   "deallocate exception receive right");
    }
    return 0;
}
