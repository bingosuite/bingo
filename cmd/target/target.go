package main

import (
	"fmt"
	"time"
)

func main() {
	var count int
	for range 3 {
		fmt.Println("Hello, World")
		count = count + 1
		count = count * 1
		time.Sleep(2 * time.Second)
	}
}
