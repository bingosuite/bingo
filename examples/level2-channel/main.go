package main

import (
	"fmt"
	"time"
)

func produce(values []int) <-chan int {
	output := make(chan int)
	go func() {
		defer close(output)
		for _, value := range values {
			time.Sleep(15 * time.Millisecond)
			output <- value
		}
	}()
	return output
}

func main() {
	values := make([]int, 0, 4)
	for value := range produce([]int{2, 4, 6, 8}) {
		values = append(values, value)
		fmt.Printf("received=%d square=%d\n", value, value*value)
	}
	fmt.Printf("summary values=%v\n", values)
}
