package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func generate(ctx context.Context, values []int, output chan<- int, stages *sync.WaitGroup) {
	defer stages.Done()
	defer close(output)
	for _, value := range values {
		select {
		case output <- value:
		case <-ctx.Done():
			return
		}
	}
}

func square(ctx context.Context, input <-chan int, output chan<- int, stages *sync.WaitGroup) {
	defer stages.Done()
	defer close(output)
	for value := range input {
		time.Sleep(10 * time.Millisecond)
		select {
		case output <- value * value:
		case <-ctx.Done():
			return
		}
	}
}

func retainEven(ctx context.Context, input <-chan int, output chan<- int, stages *sync.WaitGroup) {
	defer stages.Done()
	defer close(output)
	for value := range input {
		if value%2 != 0 {
			continue
		}
		select {
		case output <- value:
		case <-ctx.Done():
			return
		}
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	generated := make(chan int)
	squared := make(chan int)
	retained := make(chan int)
	var stages sync.WaitGroup
	stages.Add(3)
	go generate(ctx, []int{1, 2, 3, 4, 5, 6, 7, 8}, generated, &stages)
	go square(ctx, generated, squared, &stages)
	go retainEven(ctx, squared, retained, &stages)

	values := make([]int, 0, 3)
	for value := range retained {
		values = append(values, value)
		fmt.Printf("retained=%d\n", value)
		if len(values) == 3 {
			cancel()
			break
		}
	}
	stages.Wait()
	fmt.Printf("summary retained=%v canceled=%t\n", values, ctx.Err() != nil)
}
