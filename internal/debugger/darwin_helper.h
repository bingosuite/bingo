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

// Port and task management
mach_port_t get_mach_task_self(void);

// Exception port configuration
kern_return_t set_debug_exception_ports(task_t task, mach_port_t exc_port);
kern_return_t clear_debug_exception_ports(task_t task);

// Thread management
kern_return_t get_first_thread(task_t task, thread_act_t *out_thread);
kern_return_t set_thread_exception_ports(task_t task, mach_port_t port);

// Thread state operations
kern_return_t get_arm64_thread_state(thread_act_t thr, arm_thread_state64_t *state,
                                     mach_msg_type_number_t *count);
kern_return_t set_arm64_thread_state(thread_act_t thr, arm_thread_state64_t *state,
                                     mach_msg_type_number_t count);

// Memory read/write operations
kern_return_t read_word(task_t task, mach_vm_address_t addr, uint32_t *out);
kern_return_t write_word(task_t task, mach_vm_address_t addr, uint32_t val);

// Memory management and slide detection
kern_return_t find_image_slide(task_t task, mach_vm_address_t *slide);

// Exception message utilities
thread_act_t exc_msg_thread(exc_msg_t *msg);
mach_msg_bits_t make_reply_bits(mach_msg_bits_t bits);
mach_msg_id_t make_reply_id(mach_msg_id_t id);

// Single-step mode control
kern_return_t enable_single_step(thread_act_t thread);
kern_return_t disable_single_step(thread_act_t thread);

#endif  // DARWIN_HELPER_H