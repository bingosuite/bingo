package integration

import (
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	churnWatchdogPlaceholder  = "__BINGO_CHURN_WATCHDOG_SECONDS__"
	churnWatchdogPerIteration = 2 * time.Second
	churnWatchdogMargin       = 60 * time.Second
)

// churnTargetSrcTemplate forces continuous OS-thread creation/teardown
// (LockOSThread + short sleeps across GOMAXPROCS worker goroutines) so that
// breakpoint stops and single-steps happen in a genuinely multi-threaded
// context. This reproduces the darwin single-step race and stresses linux
// clone/thread-exit handling.
const churnTargetSrcTemplate = `package main

import (
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

var sink int64

func churn() {
	for {
		runtime.LockOSThread()
		var x int64
		for i := 0; i < 2000; i++ {
			x += int64(i)
		}
		atomic.AddInt64(&sink, x)
		runtime.UnlockOSThread()
		time.Sleep(50 * time.Microsecond)
	}
}

func work(n int) int64 {
	var s int64
	for i := 0; i < n; i++ {
		s += int64(i)
	}
	return s
}

func main() {
	go func() { time.Sleep(__BINGO_CHURN_WATCHDOG_SECONDS__ * time.Second); os.Exit(0) }()
	runtime.GOMAXPROCS(4)
	for i := 0; i < 8; i++ {
		go churn()
	}
	x := int64(0)
	for i := 0; i < 1000000; i++ {
		x += work(i % 50) // BP
		x++
		time.Sleep(time.Millisecond)
		_ = x
	}
}
`

func churnWatchdogBudget(iterations int) time.Duration {
	if iterations < 0 {
		iterations = 0
	}
	return time.Duration(iterations)*churnWatchdogPerIteration + churnWatchdogMargin
}

func churnTargetSource(iterations int) (string, time.Duration) {
	budget := churnWatchdogBudget(iterations)
	seconds := strconv.FormatInt(int64(budget/time.Second), 10)
	return strings.Replace(churnTargetSrcTemplate, churnWatchdogPlaceholder, seconds, 1), budget
}

func TestChurnTargetSource(t *testing.T) {
	if count := strings.Count(churnTargetSrcTemplate, churnWatchdogPlaceholder); count != 1 {
		t.Fatalf("watchdog placeholder count = %d, want 1", count)
	}

	tests := []struct {
		name        string
		iterations  int
		wantBudget  time.Duration
		wantSeconds string
	}{
		{name: "default", iterations: 200, wantBudget: 460 * time.Second, wantSeconds: "460"},
		{name: "scaled", iterations: 400, wantBudget: 860 * time.Second, wantSeconds: "860"},
		{name: "negative", iterations: -1, wantBudget: 60 * time.Second, wantSeconds: "60"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, budget := churnTargetSource(tt.iterations)
			if budget != tt.wantBudget {
				t.Fatalf("watchdog budget = %s, want %s", budget, tt.wantBudget)
			}
			if strings.Contains(source, churnWatchdogPlaceholder) {
				t.Fatal("rendered source still contains watchdog placeholder")
			}
			if !strings.Contains(source, "time.Sleep("+tt.wantSeconds+" * time.Second)") {
				t.Fatalf("rendered source does not contain %s-second watchdog", tt.wantSeconds)
			}
			if !strings.Contains(source, "i % 50") {
				t.Fatal("rendering changed the target's modulo expression")
			}
			if _, err := parser.ParseFile(token.NewFileSet(), "churn_target.go", source, 0); err != nil {
				t.Fatalf("rendered source is invalid Go: %v", err)
			}
		})
	}
}
