#ifndef DARWIN_HELPER_H
#define DARWIN_HELPER_H

#include <mach/arm/thread_state.h>
#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <stdint.h>
#include <sys/types.h>

typedef struct {
    mach_msg_header_t Head;
    mach_msg_body_t msgh_body;
    mach_msg_ool_ports_descriptor_t ool_ports;
    NDR_record_t nondescript;
    exception_type_t exception;
    mach_msg_type_number_t code_count;
    integer_t codes[2];
    mach_port_t thread_port;
    mach_port_t task_port;
} exc_msg_t;

typedef struct {
    mach_msg_header_t Head;
    NDR_record_t nondescript;
    kern_return_t RetCode;
} exc_msg_reply_t;

mach_port_t get_mach_task_self(void);
mach_msg_bits_t get_reply_bits(mach_msg_bits_t bits);
kern_return_t set_debug_exception_ports(task_t task, mach_port_t exc_port);
kern_return_t clear_debug_exception_ports(task_t task);
kern_return_t get_first_thread(task_t task, thread_act_t *out_thread);
kern_return_t get_arm64_thread_state(thread_act_t thr, arm_thread_state64_t *state, mach_msg_type_number_t *count);
kern_return_t set_arm64_thread_state(thread_act_t thr, arm_thread_state64_t *state, mach_msg_type_number_t count);
kern_return_t read_word(task_t task, mach_vm_address_t addr, uint32_t *out);
kern_return_t write_word(task_t task, mach_vm_address_t addr, uint32_t val);
kern_return_t suspend_thread(thread_act_t thread);
kern_return_t resume_thread(thread_act_t thread);
kern_return_t get_main_image_address(task_t task, mach_vm_address_t *addr);
kern_return_t find_image_slide(task_t task, mach_vm_address_t *slide);
int ptrace_attach_exc(int pid);

#endif