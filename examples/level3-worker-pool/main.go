package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

type jobResult struct {
	job    int
	square int
}

func worker(id int, jobs <-chan int, results chan<- jobResult, wg *sync.WaitGroup) {
	defer wg.Done()
	for job := range jobs {
		time.Sleep(time.Duration(5+job) * time.Millisecond)
		result := jobResult{job: job, square: job * job}
		fmt.Printf("worker=%d job=%d square=%d\n", id, result.job, result.square)
		results <- result
	}
}

func main() {
	const (
		workerCount = 3
		jobCount    = 6
	)

	jobs := make(chan int, jobCount)
	results := make(chan jobResult, jobCount)
	var workers sync.WaitGroup

	for id := 1; id <= workerCount; id++ {
		workers.Add(1)
		go worker(id, jobs, results, &workers)
	}
	for job := 1; job <= jobCount; job++ {
		jobs <- job
	}
	close(jobs)

	workers.Wait()
	close(results)

	ordered := make([]jobResult, 0, jobCount)
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].job < ordered[j].job
	})
	for _, result := range ordered {
		fmt.Printf("summary job=%d square=%d\n", result.job, result.square)
	}
}
