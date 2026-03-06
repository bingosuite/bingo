#include "darwin_helper.h"

#include <string.h>

mach_port_t get_mach_task_self(void) {
    return mach_task_self();
}

mach_msg_bits_t get_reply_bits(mach_msg_bits_t bits) {
    return MACH_MSGH_BITS(MACH_MSGH_BITS_REMOTE(bits), 0);
}

kern_return_t set_debug_exception_ports(task_t task, mach_port_t exc_port) {
    return task_set_exception_ports(
        task,
        EXC_MASK_BREAKPOINT | EXC_MASK_BAD_INSTRUCTION,
        exc_port,
        EXCEPTION_DEFAULT | MACH_EXCEPTION_CODES,
        THREAD_STATE_NONE
    );
}

kern_return_t clear_debug_exception_ports(task_t task) {
    return task_set_exception_ports(
        task,
        EXC_MASK_BREAKPOINT | EXC_MASK_BAD_INSTRUCTION,
        MACH_PORT_NULL,
        0,
        THREAD_STATE_NONE
    );
}

kern_return_t get_first_thread(task_t task, thread_act_t *out_thread) {
    thread_act_array_t thread_list;
    mach_msg_type_number_t thread_count;
    kern_return_t kr = task_threads(task, &thread_list, &thread_count);
    if (kr != KERN_SUCCESS) {
        return kr;
    }
    if (thread_count == 0) {
        vm_deallocate(mach_task_self(), (vm_address_t)thread_list, 0);
        return KERN_FAILURE;
    }

    *out_thread = thread_list[0];
    vm_deallocate(
        mach_task_self(),
        (vm_address_t)thread_list,
        (vm_size_t)(thread_count * sizeof(thread_act_t))
    );
    return KERN_SUCCESS;
}

kern_return_t get_arm64_thread_state(thread_act_t thr, arm_thread_state64_t *state, mach_msg_type_number_t *count) {
    *count = ARM_THREAD_STATE64_COUNT;
    return thread_get_state(thr, ARM_THREAD_STATE64, (thread_state_t)state, count);
}

kern_return_t set_arm64_thread_state(thread_act_t thr, arm_thread_state64_t *state, mach_msg_type_number_t count) {
    return thread_set_state(thr, ARM_THREAD_STATE64, (thread_state_t)state, count);
}

kern_return_t read_word(task_t task, mach_vm_address_t addr, uint32_t *out) {
    vm_offset_t data;
    mach_msg_type_number_t sz;

    kern_return_t kr = mach_vm_protect(task, addr & ~0x3FFF, 0x4000, 0, VM_PROT_READ | VM_PROT_WRITE | VM_PROT_EXECUTE);
    if (kr != KERN_SUCCESS) {
        return kr;
    }

    kr = mach_vm_read(task, addr, sizeof(uint32_t), &data, &sz);
    if (kr != KERN_SUCCESS) {
        mach_vm_protect(task, addr & ~0x3FFF, 0x4000, 0, VM_PROT_READ | VM_PROT_EXECUTE);
        return kr;
    }

    if (sz < sizeof(uint32_t)) {
        mach_vm_deallocate(mach_task_self(), data, sz);
        mach_vm_protect(task, addr & ~0x3FFF, 0x4000, 0, VM_PROT_READ | VM_PROT_EXECUTE);
        return KERN_FAILURE;
    }

    memcpy(out, (void *)data, sizeof(uint32_t));
    mach_vm_deallocate(mach_task_self(), data, sz);

    mach_vm_protect(task, addr & ~0x3FFF, 0x4000, 0, VM_PROT_READ | VM_PROT_EXECUTE);
    return KERN_SUCCESS;
}

kern_return_t write_word(task_t task, mach_vm_address_t addr, uint32_t val) {
    kern_return_t kr = mach_vm_protect(task, addr & ~0x3FFF, 0x4000, 0, VM_PROT_READ | VM_PROT_WRITE | VM_PROT_EXECUTE);
    if (kr != KERN_SUCCESS) {
        return kr;
    }

    kr = mach_vm_write(task, addr, (vm_offset_t)&val, sizeof(uint32_t));
    mach_vm_protect(task, addr & ~0x3FFF, 0x4000, 0, VM_PROT_READ | VM_PROT_EXECUTE);
    return kr;
}

kern_return_t suspend_thread(thread_act_t thread) {
    return thread_suspend(thread);
}

kern_return_t resume_thread(thread_act_t thread) {
    return thread_resume(thread);
}
