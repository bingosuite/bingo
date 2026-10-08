package dap

import (
	"fmt"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

type threadTarget struct {
	goroutineID int
}

type threadsReq struct {
	seq        int
	generation uint64
}

type bingoThread struct {
	godap.Thread
	GoroutineID int `json:"bingoGoroutineId,omitempty"`
}

type bingoThreadsResponse struct {
	godap.Response
	Body struct {
		Threads []bingoThread `json:"threads"`
	} `json:"body"`
}

func (h *Handler) threadForGoroutineLocked(goid int) (int, error) {
	if goid < 0 || uint64(goid) > 1<<53-1 {
		return 0, fmt.Errorf("invalid goroutine id %d", goid)
	}
	if id := h.goroutineThreads[goid]; id != 0 {
		return id, nil
	}
	id, err := h.allocVarRef()
	if err != nil {
		return 0, err
	}
	if h.threadHandles == nil {
		h.threadHandles = make(map[int]threadTarget)
		h.goroutineThreads = make(map[int]int)
	}
	h.threadHandles[id] = threadTarget{goroutineID: goid}
	h.goroutineThreads[goid] = id
	return id, nil
}

func (h *Handler) stoppedThreadLocked() int {
	if h.stopThreadUnknown {
		return 0
	}
	return h.curThreadID
}

func (h *Handler) inspectionThreadsLocked(gs []protocol.Goroutine, collapse bool) ([]bingoThread, error) {
	current := protocol.Goroutine{ID: h.curGoroutineID}
	for _, g := range gs {
		if g.Current && g.ID > 0 {
			current = g
			break
		}
	}
	currentID, err := h.threadForGoroutineLocked(current.ID)
	if err != nil {
		return nil, err
	}
	h.curThreadID, h.curGoroutineID = currentID, current.ID
	h.stopThreadUnknown = current.ID == 0
	currentThread := bingoThread{
		Thread:      godap.Thread{Id: currentID, Name: dapThreadName(current)},
		GoroutineID: current.ID,
	}
	if current.ID == 0 {
		currentThread.Name = "stopped goroutine (unknown)"
	}
	if collapse || len(gs) == 0 {
		return []bingoThread{currentThread}, nil
	}
	threads := make([]bingoThread, 0, len(gs)+1)
	if current.ID == 0 {
		threads = append(threads, currentThread)
	}
	seen := make(map[int]bool, len(gs))
	for _, g := range gs {
		if g.ID <= 0 || seen[g.ID] {
			continue
		}
		id, err := h.threadForGoroutineLocked(g.ID)
		if err != nil {
			return nil, err
		}
		seen[g.ID] = true
		threads = append(threads, bingoThread{
			Thread: godap.Thread{Id: id, Name: dapThreadName(g)}, GoroutineID: g.ID,
		})
	}
	if current.ID > 0 && !seen[current.ID] {
		threads = append(threads, currentThread)
	}
	return threads, nil
}

func (h *Handler) sendThreads(seq int, threads []bingoThread) {
	response := &bingoThreadsResponse{Response: h.response(seq, "threads")}
	response.Body.Threads = threads
	h.send(response)
}
