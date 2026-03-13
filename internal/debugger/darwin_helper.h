//go:build darwin && arm64

#ifndef DARWIN_HELPER_H
#define DARWIN_HELPER_H

#include <mach/arm/thread_state.h>
#include <mach/arm/thread_status.h>
#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <mach/exception_types.h>
#include <mach/message.h>
#include <stdint.h>
#include <sys/types.h>

typedef struct {
    mach_msg_header_t Head;
    mach_msg_body_t msgh_body;

    mach_msg_port_descriptor_t thread;
    mach_msg_port_descriptor_t task;

    NDR_record_t NDR;

    exception_type_t exception;
    mach_msg_type_number_t codeCnt;
    mach_exception_data_type_t code[2];

} exc_msg_t;

typedef struct {
    mach_msg_header_t Head;
    NDR_record_t nondescript;
    kern_return_t RetCode;
} exc_msg_reply_t;

enum {
    sizeof_exc_msg_reply_t = sizeof(exc_msg_reply_t)
};

// Returns the current process's Mach task port.
mach_port_t get_mach_task_self(void);
// Releases send and receive rights for an exception port.
kern_return_t cleanup_exception_port(mach_port_t port);

// Installs breakpoint and bad-instruction exception handlers for a task.
kern_return_t set_debug_exception_ports(task_t task, mach_port_t exc_port);
// Clears breakpoint and bad-instruction exception handlers for a task.
kern_return_t clear_debug_exception_ports(task_t task);

// Returns the first thread in a task.
kern_return_t get_first_thread(task_t task, thread_act_t *out_thread);
// Applies exception port settings to all threads in a task.
kern_return_t set_thread_exception_ports(task_t task, mach_port_t port);

// Thread state operations
// Reads ARM64 general-purpose register state for a thread.
kern_return_t get_arm64_thread_state(thread_act_t thr, arm_thread_state64_t *state,
                                     mach_msg_type_number_t *count);
// Writes ARM64 general-purpose register state for a thread.
kern_return_t set_arm64_thread_state(thread_act_t thr, arm_thread_state64_t *state,
                                     mach_msg_type_number_t count);

// Reads a 32-bit word from target task memory.
kern_return_t read_word(task_t task, mach_vm_address_t addr, uint32_t *out);
// Writes a 32-bit word to target task memory, adjusting page protections as needed.
kern_return_t write_word(task_t task, mach_vm_address_t addr, uint32_t val);
// Checks whether a target address is readable by attempting a 32-bit read.
kern_return_t probe_address_readable(task_t task, mach_vm_address_t addr);

// Finds the main executable image in a task and computes its ASLR slide.
kern_return_t find_image_slide(task_t task, mach_vm_address_t *slide);

// Extracts the thread port from an exception message.
thread_act_t exc_msg_thread(exc_msg_t *msg);
// Builds reply message bits using the original message's remote bits.
mach_msg_bits_t make_reply_bits(mach_msg_bits_t bits);
// Builds the corresponding Mach exception reply message ID.
mach_msg_id_t make_reply_id(mach_msg_id_t id);

// Enables hardware single-step mode for a thread.
kern_return_t enable_single_step(thread_act_t thread);
// Disables hardware single-step mode for a thread.
kern_return_t disable_single_step(thread_act_t thread);

#endif  // DARWIN_HELPER_H