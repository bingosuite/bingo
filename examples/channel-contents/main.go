package main

import "fmt"

type job struct {
	Name string
	ID   int
}

func main() {
	jobs := make(chan job, 3)
	jobs <- job{Name: "discard"}
	jobs <- job{Name: "first", ID: 10}
	jobs <- job{Name: "second", ID: 20}
	<-jobs
	jobs <- job{Name: "third", ID: 30}
	close(jobs)
	fmt.Printf("inspect jobs: len=%d cap=%d\n", len(jobs), cap(jobs))
	for value := range jobs {
		fmt.Printf("%s=%d\n", value.Name, value.ID)
	}
}
