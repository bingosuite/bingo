// Command spawntree is a concurrency demo target for bingo's telemetry stream.
//
// It builds a small, deterministic goroutine spawn tree — main → supervisor →
// worker×N — that stays alive across breakpoint stops, so the goroutine/thread
// snapshot stream has something meaningful to show: parent linkage (the go
// statement that spawned each worker), created/exited lifecycle deltas as rounds
// churn workers, and a live thread set.
//
// Set a breakpoint on the marked line inside worker and drive execution over DAP
// (VS Code or cmd/dapcli) while watching the spawn tree update in cmd/wsmon.
// See docs/ConcurrencyTelemetry.md for the end-to-end runbook.
package main

import (
	"fmt"
	"sync"
	"time"
)

// worker consumes jobs until the channel closes. The Printf line is the intended
// breakpoint site: several workers park there under one supervisor, which is what
// makes the spawn tree and the current-goroutine marker interesting to observe.
func worker(id int, jobs <-chan int, wg *sync.WaitGroup) {
	defer wg.Done()
	for j := range jobs {
		result := j * j
		fmt.Printf("worker %d processed job %d -> %d\n", id, j, result) // <-- breakpoint here
		time.Sleep(150 * time.Millisecond)
	}
}

// supervisor spawns a fresh pool of workers each round and feeds them jobs. Each
// round's workers exit when the round's channel closes, so consecutive snapshots
// show workers in the Exited delta and the next round's workers in Created.
func supervisor(rounds, workers, jobsPerRound int) {
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		jobs := make(chan int)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go worker(w, jobs, &wg)
		}
		for j := 0; j < jobsPerRound; j++ {
			jobs <- j
		}
		close(jobs)
		wg.Wait()
		time.Sleep(300 * time.Millisecond)
	}
}

func main() {
	const (
		rounds       = 8
		workers      = 3
		jobsPerRound = 6
	)

	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		supervisor(rounds, workers, jobsPerRound)
	}()
	done.Wait()

	fmt.Println("spawntree: done")
}
