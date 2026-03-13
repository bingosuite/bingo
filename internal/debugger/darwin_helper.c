//go:build darwin && arm64

#include "darwin_helper.h"

#include <mach/arm/thread_status.h>
#include <mach/exc.h>
#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <mach/thread_act.h>
#include <mach/task_info.h>
#include <mach-o/loader.h>
#include <stdio.h>
#include <string.h>
#include <sys/ptrace.h>

// Finds the main executable image in a task and computes its ASLR slide.
kern_return_t find_image_slide(task_t task, mach_vm_address_t *slide) {
    mach_vm_address_t addr = MACH_VM_MIN_ADDRESS;
    mach_vm_size_t region_size = 0;

    while (1) {
        vm_region_submap_info_data_64_t info;
        mach_msg_type_number_t count = VM_REGION_SUBMAP_INFO_COUNT_64;
        natural_t depth = 0;

        kern_return_t kr = mach_vm_region_recurse(
            task,
            &addr,
            &region_size,
            &depth,
            (vm_region_recurse_info_t)&info,
            &count
        );

        if (kr != KERN_SUCCESS)
            return kr;

        if (info.protection & VM_PROT_EXECUTE) {
            struct mach_header_64 mh;
            mach_vm_size_t read_size = 0;
            kr = mach_vm_read_overwrite(
                task,
                addr,
                sizeof(mh),
                (mach_vm_address_t)&mh,
                &read_size
            );

            if (kr == KERN_SUCCESS && read_size >= sizeof(mh) && mh.magic == MH_MAGIC_64 && mh.filetype == MH_EXECUTE) {
                // Compute unslid vmaddr from first LC_SEGMENT_64 with fileoff==0.
                // This corresponds to where Mach-O expects the image to be loaded.
                uint8_t lc_buf[4096];
                mach_vm_size_t lc_size = sizeof(lc_buf);
                mach_vm_address_t lc_addr = addr + sizeof(struct mach_header_64);

                kr = mach_vm_read_overwrite(
                    task,
                    lc_addr,
                    lc_size,
                    (mach_vm_address_t)lc_buf,
                    &lc_size
                );

                if (kr == KERN_SUCCESS) {
                    uint32_t ncmds = mh.ncmds;
                    uint32_t sizeofcmds = mh.sizeofcmds;
                    if (sizeofcmds > lc_size) {
                        sizeofcmds = (uint32_t)lc_size;
                    }

                    uint32_t off = 0;
                    for (uint32_t i = 0; i < ncmds && off + sizeof(struct load_command) <= sizeofcmds; i++) {
                        struct load_command *lc = (struct load_command *)(lc_buf + off);
                        if (lc->cmdsize == 0 || off + lc->cmdsize > sizeofcmds) {
                            break;
                        }

                        if (lc->cmd == LC_SEGMENT_64 && lc->cmdsize >= sizeof(struct segment_command_64)) {
                            struct segment_command_64 *seg = (struct segment_command_64 *)lc;
                            // Use __TEXT as the unslid image base. Using only fileoff==0 can
                            // incorrectly match __PAGEZERO (vmaddr=0), which produces a huge,
                            // invalid slide and wrong runtime breakpoint addresses.
                            if (strncmp(seg->segname, "__TEXT", 16) == 0) {
                                *slide = addr - seg->vmaddr;
                                return KERN_SUCCESS;
                            }
                        }

                        off += lc->cmdsize;
                    }
                }
            }
        }

        addr += region_size;
    }
}

// Returns the current process's Mach task port.
mach_port_t get_mach_task_self(void) {
    return mach_task_self();
}

// Releases send and receive rights for an exception port.
kern_return_t cleanup_exception_port(mach_port_t port) {
    kern_return_t kr;

    // Remove send right
    kr = mach_port_mod_refs(mach_task_self(), port, MACH_PORT_RIGHT_SEND, -1);
    if (kr != KERN_SUCCESS) return kr;

    // Remove receive right
    kr = mach_port_mod_refs(mach_task_self(), port, MACH_PORT_RIGHT_RECEIVE, -1);
    return kr;
}

// Installs breakpoint and bad-instruction exception handlers for a task.
kern_return_t set_debug_exception_ports(task_t task, mach_port_t exc_port) {
    return task_set_exception_ports(
        task,
        EXC_MASK_BREAKPOINT | EXC_MASK_BAD_INSTRUCTION,
        exc_port,
        EXCEPTION_DEFAULT | MACH_EXCEPTION_CODES,
        THREAD_STATE_NONE
    );
}

// Clears breakpoint and bad-instruction exception handlers for a task.
kern_return_t clear_debug_exception_ports(task_t task) {
    return task_set_exception_ports(
        task,
        EXC_MASK_BREAKPOINT | EXC_MASK_BAD_INSTRUCTION,
        MACH_PORT_NULL,
        0,
        THREAD_STATE_NONE
    );
}

// Returns the first thread in a task.
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

    // task_threads returns send rights for each thread. Keep only the selected
    // first thread right and release the rest to avoid leaking rights.
    for (mach_msg_type_number_t i = 1; i < thread_count; i++) {
        mach_port_deallocate(mach_task_self(), thread_list[i]);
    }

    vm_deallocate(
        mach_task_self(),
        (vm_address_t)thread_list,
        (vm_size_t)(thread_count * sizeof(thread_act_t))
    );
    return KERN_SUCCESS;
}

// Reads ARM64 general-purpose register state for a thread.
kern_return_t get_arm64_thread_state(thread_act_t thr, arm_thread_state64_t *state,
                                     mach_msg_type_number_t *count) {
    *count = ARM_THREAD_STATE64_COUNT;
    return thread_get_state(thr, ARM_THREAD_STATE64, (thread_state_t)state, count);
}

// Writes ARM64 general-purpose register state for a thread.
kern_return_t set_arm64_thread_state(thread_act_t thr, arm_thread_state64_t *state,
                                     mach_msg_type_number_t count) {
    return thread_set_state(thr, ARM_THREAD_STATE64, (thread_state_t)state, count);
}

// Reads a 32-bit word from the target task memory.
kern_return_t read_word(task_t task, mach_vm_address_t addr, uint32_t *out) {
    mach_vm_size_t outsize = 0;

    return mach_vm_read_overwrite(
        task,
        addr,
        sizeof(uint32_t),
        (mach_vm_address_t)out,
        &outsize
    );
}

// Writes a 32-bit word to target task memory, adjusting page protections as needed.
kern_return_t write_word(task_t task, mach_vm_address_t addr, uint32_t val) {
    mach_vm_address_t page = addr & ~0xFFF;
    mach_vm_size_t size = 0x1000;

    kern_return_t kr;

    kr = mach_vm_protect(task, page, size, FALSE,
                         VM_PROT_READ | VM_PROT_WRITE);
    if (kr != KERN_SUCCESS) {
        printf("mach_vm_protect(RW) failed: %d\n", kr);
        return kr;
    }

    kr = mach_vm_write(task, addr, (vm_offset_t)&val, sizeof(uint32_t));
    if (kr != KERN_SUCCESS) {
        printf("mach_vm_write failed: %d\n", kr);
        return kr;
    }

    kr = mach_vm_protect(task, page, size, FALSE, VM_PROT_READ | VM_PROT_EXECUTE);

    return kr;
}

// Checks whether a target address is readable by attempting a 32-bit read.
kern_return_t probe_address_readable(task_t task, mach_vm_address_t addr) {
    uint32_t tmp = 0;
    mach_vm_size_t outsize = 0;
    return mach_vm_read_overwrite(
        task,
        addr,
        sizeof(uint32_t),
        (mach_vm_address_t)&tmp,
        &outsize
    );
}

// Applies exception port settings to all threads in a task.
kern_return_t set_thread_exception_ports(task_t task, mach_port_t port) {
    thread_act_array_t threads;
    mach_msg_type_number_t count;

    kern_return_t kr = task_threads(task, &threads, &count);
    if (kr != KERN_SUCCESS) return kr;

    kern_return_t first_error = KERN_SUCCESS;

    for (mach_msg_type_number_t i = 0; i < count; i++) {
        kern_return_t set_kr = thread_set_exception_ports(
            threads[i],
            EXC_MASK_BREAKPOINT | EXC_MASK_BAD_INSTRUCTION,
            port,
            EXCEPTION_DEFAULT | MACH_EXCEPTION_CODES,
            ARM_THREAD_STATE64
        );

        if (set_kr != KERN_SUCCESS && first_error == KERN_SUCCESS) {
            first_error = set_kr;
        }

        mach_port_deallocate(mach_task_self(), threads[i]);
    }

    vm_deallocate(mach_task_self(), (vm_address_t)threads, count * sizeof(thread_act_t));
    return first_error;
}

// Exception message utilities
// Extracts the thread port from an exception message.
thread_act_t exc_msg_thread(exc_msg_t *msg) {
    return msg->thread.name;
}

// Builds reply message bits using the original message's remote bits.
mach_msg_bits_t make_reply_bits(mach_msg_bits_t bits) {
    return MACH_MSGH_BITS(MACH_MSGH_BITS_REMOTE(bits), 0);
}

// Builds the corresponding Mach exception reply message ID.
mach_msg_id_t make_reply_id(mach_msg_id_t id) {
    return id + 100;
}

// Destroys a received Mach message and releases any descriptor rights it carries.
void destroy_mach_message(mach_msg_header_t *msg) {
    mach_msg_destroy(msg);
}

// Releases a send right for a thread port when it is no longer needed.
kern_return_t release_thread_port(thread_act_t thread) {
    return mach_port_deallocate(mach_task_self(), thread);
}

// Single-step mode control
// Enable single-step mode (sets SS bit in MDSCR_EL1)
kern_return_t enable_single_step(thread_act_t thread) {
    arm_debug_state64_t debug_state;
    mach_msg_type_number_t count = ARM_DEBUG_STATE64_COUNT;

    kern_return_t kr = thread_get_state(thread, ARM_DEBUG_STATE64,
                                        (thread_state_t)&debug_state, &count);
    if (kr != KERN_SUCCESS) {
        return kr;
    }

    // Set bit 0 (SS - Single Step) in MDSCR_EL1
    debug_state.__mdscr_el1 |= 1;

    return thread_set_state(thread, ARM_DEBUG_STATE64,
                            (thread_state_t)&debug_state, ARM_DEBUG_STATE64_COUNT);
}

// Disable single-step mode (clears SS bit in MDSCR_EL1)
kern_return_t disable_single_step(thread_act_t thread) {
    arm_debug_state64_t debug_state;
    mach_msg_type_number_t count = ARM_DEBUG_STATE64_COUNT;

    kern_return_t kr = thread_get_state(thread, ARM_DEBUG_STATE64,
                                        (thread_state_t)&debug_state, &count);
    if (kr != KERN_SUCCESS) {
        return kr;
    }

    // Clear bit 0 (SS - Single Step) in MDSCR_EL1
    debug_state.__mdscr_el1 &= ~1;

    return thread_set_state(thread, ARM_DEBUG_STATE64,
                            (thread_state_t)&debug_state, ARM_DEBUG_STATE64_COUNT);
}