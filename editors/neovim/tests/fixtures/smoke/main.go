package main

import (
	"os"
	"runtime"
	"strconv"
	"time"
)

func main() {
	time.AfterFunc(90*time.Second, func() { os.Exit(99) })
	if err := os.WriteFile(os.Getenv("BINGO_NVIM_SMOKE_PID_FILE"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		panic(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Getenv("BINGO_NVIM_SMOKE_CWD_FILE"), []byte(cwd), 0600); err != nil {
		panic(err)
	}
	known := 42
	known++ // NVIM_SMOKE_BREAKPOINT
	runtime.KeepAlive(known)
	for {
		time.Sleep(time.Second)
	}
}
