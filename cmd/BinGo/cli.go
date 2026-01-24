package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func resume(pid int) bool {
	sub := false
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("\n(C)ontinue, (S)tep, set (B)reakpoint or (Q)uit >")
	for {
		scanner.Scan()
		input := scanner.Text()
		switch strings.ToUpper(input) {
		case "C":
			return true
		case "S":
			return false
		case "B":
			fmt.Printf("\nEnter line number in %s: >", targetFile)
			sub = true
		case "Q":
			os.Exit(0)
		default:
			if sub {
				line, _ = strconv.Atoi(input)
				breakpointSet, originalCode = setBreak(pid, targetFile, line)
				return true
			}
			fmt.Printf("Unexpected input %s\n", input)
			fmt.Printf("\n(C)ontinue, (S)tep, set (B)reakpoint or (Q)uit? > ")
		}
	}
}

/*func outputStack(symTable *gosym.Table, pid int, ip uint64, sp uint64, bp uint64) {

	// ip = Instruction Pointer
	// sp = Stack Pointer
	// bp = Base(Frame) Pointer

	_, _, fn = symTable.PCToLine(ip)
	var i uint64
	var nextbp uint64

	for {

		// Only works if stack frame is [Return Address]
		//								[locals]
		//								[Saved RBP]
		i = 0
		frameSize := bp - sp + 8

		//Can happen when we look at bp and sp while they're being updated
		if frameSize > 1000 || bp == 0 {
			fmt.Printf("Weird frame size: SP: %X | BP: %X \n", sp, bp)
			frameSize = 32
			bp = sp + frameSize - 8
		}

		// Read stack memory at sp into b
		b := make([]byte, frameSize)
		_, err := syscall.PtracePeekData(pid, uintptr(sp), b)
		if err != nil {
			panic(err)
		}

		// Reads return address into content
		content := binary.LittleEndian.Uint64((b[i : i+8]))
		_, lineno, nextfn := symTable.PCToLine(content)
		if nextfn != nil {
			fn = nextfn
			fmt.Printf("  called by %s line %d\n", fn.Name, lineno)
		}

		//Rest of the frame
		for i = 8; sp+1 <= bp; i += 8 {
			content := binary.LittleEndian.Uint64(b[i : i+8])
			if sp+i == bp {
				nextbp = content
			}
		}

		//Stop stack trace at main.main. If bp and sp are being updated we could miss main.main so we backstop with runtime.amin
		if fn.Name == "main.main" || fn.Name == "runtime.main" {
			break
		}

		sp = sp + i
		bp = nextbp
	}
}*/
