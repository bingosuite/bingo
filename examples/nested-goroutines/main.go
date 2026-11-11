package main

import (
	"fmt"
	"sync"
	"time"
)

func branch(path string, depth int, ready chan<- string, release <-chan struct{}, done *sync.WaitGroup) {
	defer done.Done()
	if depth > 1 {
		go branch(path+"/left", depth-1, ready, release, done)
		go branch(path+"/right", depth-1, ready, release, done)
	}
	ready <- path
	// Keeping parents alive preserves every level of the spawn hierarchy.
	<-release
}

func main() {
	const depth = 3
	const nodes = (1 << (depth + 1)) - 2

	for round := 1; round <= 3; round++ {
		ready := make(chan string, nodes)
		release := make(chan struct{})
		var done sync.WaitGroup
		done.Add(nodes)
		go branch("left", depth, ready, release, &done)
		go branch("right", depth, ready, release, &done)

		for range nodes {
			<-ready
		}
		fmt.Printf("round %d: all %d nested goroutines are ready\n", round, nodes)
		time.Sleep(2 * time.Second)
		close(release)
		done.Wait()
	}
	fmt.Println("nested goroutines: done")
}
