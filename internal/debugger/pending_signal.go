package debugger

import "sync"

// pendingSignals keeps Linux signal-delivery stops attached to the TID that
// produced them until that exact thread is resumed. A current stop has priority
// over SIGURG delayed through a single-step, but the two cannot overwrite each
// other. Wait writes from its one-shot goroutine while ContinueProcess and
// SingleStep consume on the engine loop, so this state needs synchronization
// even though the parked-stop FIFO does not.
type pendingSignals struct {
	mu           sync.Mutex
	currentByTID map[int]int
	delayedByTID map[int]int
}

func (p *pendingSignals) set(tid, signal int) {
	if tid == 0 || signal == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.currentByTID == nil {
		p.currentByTID = make(map[int]int)
	}
	p.currentByTID[tid] = signal
}

func (p *pendingSignals) delay(tid, signal int) {
	if tid == 0 || signal == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.delayedByTID == nil {
		p.delayedByTID = make(map[int]int)
	}
	p.delayedByTID[tid] = signal
}

func (p *pendingSignals) take(tid int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if signal := p.currentByTID[tid]; signal != 0 {
		delete(p.currentByTID, tid)
		return signal
	}
	signal := p.delayedByTID[tid]
	delete(p.delayedByTID, tid)
	return signal
}

func (p *pendingSignals) takeForContinue(tid int) (signal, delayed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	signal = p.currentByTID[tid]
	delete(p.currentByTID, tid)
	delayed = p.delayedByTID[tid]
	delete(p.delayedByTID, tid)
	if signal == 0 {
		return delayed, 0
	}
	if signal == delayed {
		return signal, 0
	}
	return signal, delayed
}

func (p *pendingSignals) takeForStep(tid int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	signal := p.currentByTID[tid]
	delete(p.currentByTID, tid)
	return signal
}

func (p *pendingSignals) restore(tid, signal, delayed int) {
	if tid == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if signal != 0 {
		if p.currentByTID == nil {
			p.currentByTID = make(map[int]int)
		}
		p.currentByTID[tid] = signal
	}
	if delayed != 0 {
		if p.delayedByTID == nil {
			p.delayedByTID = make(map[int]int)
		}
		p.delayedByTID[tid] = delayed
	}
}

func (p *pendingSignals) takeForExplicitResume(tid, signal int) (resumeSignal, delayed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.currentByTID, tid)
	delayed = p.delayedByTID[tid]
	delete(p.delayedByTID, tid)
	if signal == 0 {
		return delayed, 0
	}
	if signal == delayed {
		return signal, 0
	}
	return signal, delayed
}

func (p *pendingSignals) clear(tid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.currentByTID, tid)
	delete(p.delayedByTID, tid)
}

func (p *pendingSignals) purge() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.currentByTID = nil
	p.delayedByTID = nil
}
