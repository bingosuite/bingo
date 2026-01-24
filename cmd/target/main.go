package main

import (
	"fmt"
	"time"
)

func main() {
	//fmt.Println("Hello, World")
	var count int
	for {
		fmt.Println("Hello, World")
		count = count + 1
		count = count * 1
		time.Sleep(2 * time.Second)

	}
}
