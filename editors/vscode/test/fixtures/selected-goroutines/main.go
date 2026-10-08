package main

import "fmt"

func selectedWorker(tag int, ready chan<- int, gate <-chan struct{}) {
	label := tag
	ready <- label
	<-gate
	fmt.Println(label)
}

func main() {
	ready := make(chan int)
	gate := make(chan struct{})
	for _, label := range []int{101, 202, 303} {
		go selectedWorker(label, ready, gate)
	}
	for range 3 {
		<-ready
	}
	fmt.Println("ready")
	close(gate)
}
