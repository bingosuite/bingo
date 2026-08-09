package debugger

import "sync"

// pendingSignals keeps Linux signal-delivery stops attached to the TID that
// produced them until that exact thread is resumed. Wait writes from its
// one-shot goroutine while ContinueProcess and SingleStep consume on the engine
// loop, so this state needs synchronization even though the parked-stop FIFO
// does not.
type pendingSignals struct {
	mu    sync.Mutex
	byTID map[int]int
}

func (p *pendingSignals) set(tid, signal int) {
	if tid == 0 || signal == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byTID == nil {
		p.byTID = make(map[int]int)
	}
	p.byTID[tid] = signal
}

func (p *pendingSignals) take(tid int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	signal := p.byTID[tid]
	delete(p.byTID, tid)
	return signal
}

func (p *pendingSignals) clear(tid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byTID, tid)
}

func (p *pendingSignals) purge() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byTID = nil
}
