package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestNestedBranches(t *testing.T) {
	for depth := 1; depth <= 3; depth++ {
		t.Run(fmt.Sprintf("depth-%d", depth), func(t *testing.T) {
			nodes := (1 << (depth + 1)) - 2
			ready := make(chan string, nodes)
			release := make(chan struct{})
			var done sync.WaitGroup
			done.Add(nodes)
			exited := make(chan struct{})
			t.Cleanup(func() {
				close(release)
				select {
				case <-exited:
				case <-time.After(5 * time.Second):
					t.Error("nested branches did not exit after release")
				}
			})
			go branch("left", depth, ready, release, &done)
			go branch("right", depth, ready, release, &done)
			go func() {
				done.Wait()
				close(exited)
			}()

			paths := make(map[string]bool)
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for range nodes {
				select {
				case path := <-ready:
					if paths[path] {
						t.Errorf("duplicate branch %q", path)
					}
					paths[path] = true
				case <-deadline.C:
					t.Fatal("nested branches did not become ready")
				}
			}
			for _, root := range []string{"left", "right"} {
				assertBranchPaths(t, paths, root, depth)
			}
		})
	}
}

func assertBranchPaths(t *testing.T, paths map[string]bool, path string, depth int) {
	t.Helper()
	if !paths[path] {
		t.Errorf("missing branch %q", path)
	}
	if depth > 1 {
		assertBranchPaths(t, paths, path+"/left", depth-1)
		assertBranchPaths(t, paths, path+"/right", depth-1)
	}
}
