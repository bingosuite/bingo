package debugger

import (
	"fmt"
	"time"
)

func RunTarget() {
	var count int
	for {
		fmt.Println("Hello, World")
		count = count + 1
		count = count * 1
		time.Sleep(2 * time.Second)
	}
}
