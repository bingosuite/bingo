# BinGo

da debugga

## Documentation
### Ptrace System Calls
```Go 
func PtraceAttach(pid int) (err error) // Attach the debugger to a running process
func PtraceDetach(pid int) (err error) // Detach from process
func PtracePeekData(pid int, addr uintptr, out []byte) (count int, err error) // Read a word at the address `addr` into `out`
func PtracePokeData(pid int, addr uintptr, data []byte) (count int, err error) // Copy `data` into memory at the address `addr`
func PtraceGetRegs(pid int, regsout *PtraceRegs) (err error) // Copy the target's registers into `regsout`
func PtraceSetRegs(pid int, regs *PtraceRegs) (err error) // Copy `regs` into the target's registers
func PtraceGetEventMsg(pid int) (msg uint, err error) // Returns a message about the Ptrace event that just happened
func PtraceCont(pid int, signal int) (err error) // Resume the target. If signal != 0, send the signal associated with that number as well
func PtraceSingleStep(pid int) (err error) // Resume the target and stop after the execution of a single instruction
func PtraceSetOptions(pid int, options int) (err error) // Set different Ptrace options, look at [text](https://man7.org/linux/man-pages/man2/ptrace.2.html)
func PtraceSyscall(pid int, signal int) (err error) //  Resume the target and stop after entry to a system call or exit to a system call

```
