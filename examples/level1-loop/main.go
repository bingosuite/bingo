package main

import "fmt"

func main() {
	total := 0
	for step := 1; step <= 5; step++ {
		total += step
		fmt.Printf("step=%d total=%d\n", step, total)
	}
	fmt.Printf("summary total=%d\n", total)
}
