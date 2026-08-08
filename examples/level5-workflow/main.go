package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

type order struct {
	id              string
	items           int
	paymentRejected bool
}

type stageResult struct {
	stage  string
	amount int
	err    error
}

type ledgerEntry struct {
	orderID string
	status  string
	detail  string
	total   int
}

type ledger struct {
	mu        sync.Mutex
	entries   []ledgerEntry
	completed int
	failed    int
}

func (l *ledger) record(entry ledgerEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	if entry.status == "completed" {
		l.completed++
	} else {
		l.failed++
	}
}

func (l *ledger) report() ([]ledgerEntry, int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := append([]ledgerEntry(nil), l.entries...)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].orderID < entries[j].orderID
	})
	return entries, l.completed, l.failed
}

func paymentStage(ctx context.Context, current order, approved chan<- struct{}, results chan<- stageResult, stages *sync.WaitGroup) {
	defer stages.Done()
	time.Sleep(10 * time.Millisecond)
	if current.paymentRejected {
		results <- stageResult{stage: "payment", err: errors.New("payment rejected")}
		return
	}
	close(approved)
	select {
	case results <- stageResult{stage: "payment", amount: current.items * 10}:
	case <-ctx.Done():
	}
}

func inventoryStage(ctx context.Context, current order, approved <-chan struct{}, results chan<- stageResult, stages *sync.WaitGroup) {
	defer stages.Done()
	select {
	case <-approved:
	case <-ctx.Done():
		results <- stageResult{stage: "inventory", err: ctx.Err()}
		return
	}
	time.Sleep(15 * time.Millisecond)
	select {
	case results <- stageResult{stage: "inventory", amount: current.items}:
	case <-ctx.Done():
	}
}

func packingStage(ctx context.Context, current order, approved <-chan struct{}, results chan<- stageResult, stages *sync.WaitGroup) {
	defer stages.Done()
	select {
	case <-approved:
	case <-ctx.Done():
		results <- stageResult{stage: "packing", err: ctx.Err()}
		return
	}
	time.Sleep(20 * time.Millisecond)
	select {
	case results <- stageResult{stage: "packing", amount: current.items * 2}:
	case <-ctx.Done():
	}
}

func processOrder(current order, records *ledger, workflows *sync.WaitGroup) {
	defer workflows.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	approved := make(chan struct{})
	results := make(chan stageResult, 3)
	var stages sync.WaitGroup
	stages.Add(3)
	go paymentStage(ctx, current, approved, results, &stages)
	go inventoryStage(ctx, current, approved, results, &stages)
	go packingStage(ctx, current, approved, results, &stages)

	collected := make([]stageResult, 0, 3)
	for len(collected) < 3 {
		result := <-results
		collected = append(collected, result)
		if result.err != nil && !errors.Is(result.err, context.Canceled) {
			cancel()
		}
	}
	stages.Wait()

	sort.Slice(collected, func(i, j int) bool {
		return collected[i].stage < collected[j].stage
	})
	total := 0
	for _, result := range collected {
		if result.err != nil && !errors.Is(result.err, context.Canceled) {
			records.record(ledgerEntry{
				orderID: current.id,
				status:  "failed",
				detail:  result.err.Error(),
			})
			fmt.Printf("workflow=%s status=failed stage=%s\n", current.id, result.stage)
			return
		}
		total += result.amount
	}

	records.record(ledgerEntry{orderID: current.id, status: "completed", total: total})
	fmt.Printf("workflow=%s status=completed\n", current.id)
}

func main() {
	orders := []order{
		{id: "order-101", items: 2},
		{id: "order-202", items: 4, paymentRejected: true},
		{id: "order-303", items: 3},
	}

	var records ledger
	var workflows sync.WaitGroup
	for _, current := range orders {
		workflows.Add(1)
		go processOrder(current, &records, &workflows)
	}
	workflows.Wait()

	entries, completed, failed := records.report()
	for _, entry := range entries {
		if entry.status == "completed" {
			fmt.Printf("ledger order=%s status=%s total=%d\n", entry.orderID, entry.status, entry.total)
		} else {
			fmt.Printf("ledger order=%s status=%s detail=%q\n", entry.orderID, entry.status, entry.detail)
		}
	}
	fmt.Printf("summary completed=%d failed=%d\n", completed, failed)
}
