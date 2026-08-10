package debugger

import "sync"

// pendingSignals keeps Linux signal-delivery stops attached to the TID that
// produced them until that exact thread is resumed. The newest delivered stop
// stays current for the next PTRACE_CONT; older distinct signals displaced by a
// later stop remain in capture order ahead of SIGURG delayed through an
// interrupted single-step. Wait writes from its one-shot goroutine while
// ContinueProcess consumes on the engine loop, so this state needs
// synchronization even though the parked-stop FIFO does not.
type pendingSignals struct {
	mu           sync.Mutex
	currentByTID map[int]int
	backlogByTID map[int][]int
	delayedByTID map[int]int
}

type pendingSignalBatch struct {
	current  int
	deferred []int
}

func (b pendingSignalBatch) withoutDeferredPrefix(count int) pendingSignalBatch {
	if count >= len(b.deferred) {
		b.deferred = nil
	} else {
		b.deferred = append([]int(nil), b.deferred[count:]...)
	}
	return b
}

func (b *pendingSignalBatch) removeDeferred(signal int) {
	for i, queued := range b.deferred {
		if queued == signal {
			b.deferred = append(b.deferred[:i], b.deferred[i+1:]...)
			return
		}
	}
}

func (p *pendingSignals) set(tid, signal int) {
	if tid == 0 || signal == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if current := p.currentByTID[tid]; current != 0 {
		if current == signal {
			return
		}
		delete(p.currentByTID, tid)
		p.addBacklogLocked(tid, current)
	}
	p.removeBacklogLocked(tid, signal)
	if p.delayedByTID[tid] == signal {
		delete(p.delayedByTID, tid)
	}
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
	if p.currentByTID[tid] == signal || containsSignal(p.backlogByTID[tid], signal) {
		return
	}
	if p.delayedByTID == nil {
		p.delayedByTID = make(map[int]int)
	}
	if p.delayedByTID[tid] != 0 {
		return
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
	if backlog := p.backlogByTID[tid]; len(backlog) > 0 {
		signal := backlog[0]
		if len(backlog) == 1 {
			delete(p.backlogByTID, tid)
		} else {
			p.backlogByTID[tid] = backlog[1:]
		}
		return signal
	}
	signal := p.delayedByTID[tid]
	delete(p.delayedByTID, tid)
	return signal
}

func (p *pendingSignals) takeForContinue(tid int) pendingSignalBatch {
	p.mu.Lock()
	defer p.mu.Unlock()
	batch := p.takeBatchLocked(tid)
	return batch
}

func (p *pendingSignals) takeForExplicitResume(tid, signal int) (pendingSignalBatch, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	batch := p.takeBatchLocked(tid)
	batch.removeDeferred(signal)
	return batch, signal
}

func (p *pendingSignals) takeBatchLocked(tid int) pendingSignalBatch {
	batch := pendingSignalBatch{
		current:  p.currentByTID[tid],
		deferred: append([]int(nil), p.backlogByTID[tid]...),
	}
	if delayed := p.delayedByTID[tid]; delayed != 0 &&
		delayed != batch.current &&
		!containsSignal(batch.deferred, delayed) {
		batch.deferred = append(batch.deferred, delayed)
	}
	delete(p.currentByTID, tid)
	delete(p.backlogByTID, tid)
	delete(p.delayedByTID, tid)
	return batch
}

func (p *pendingSignals) restore(tid int, batch pendingSignalBatch) {
	if tid == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if batch.current != 0 {
		if current := p.currentByTID[tid]; current == 0 {
			p.removeBacklogLocked(tid, batch.current)
			if p.delayedByTID[tid] == batch.current {
				delete(p.delayedByTID, tid)
			}
			if p.currentByTID == nil {
				p.currentByTID = make(map[int]int)
			}
			p.currentByTID[tid] = batch.current
		} else if current != batch.current {
			p.addBacklogLocked(tid, batch.current)
		}
	}

	for _, signal := range batch.deferred {
		p.addBacklogLocked(tid, signal)
	}
}

func (p *pendingSignals) addBacklogLocked(tid, signal int) {
	if signal == 0 ||
		p.currentByTID[tid] == signal ||
		p.delayedByTID[tid] == signal ||
		containsSignal(p.backlogByTID[tid], signal) {
		return
	}
	if p.backlogByTID == nil {
		p.backlogByTID = make(map[int][]int)
	}
	p.backlogByTID[tid] = append(p.backlogByTID[tid], signal)
}

func (p *pendingSignals) removeBacklogLocked(tid, signal int) {
	backlog := p.backlogByTID[tid]
	for i, queued := range backlog {
		if queued != signal {
			continue
		}
		backlog = append(backlog[:i], backlog[i+1:]...)
		if len(backlog) == 0 {
			delete(p.backlogByTID, tid)
		} else {
			p.backlogByTID[tid] = backlog
		}
		return
	}
}

func containsSignal(signals []int, signal int) bool {
	for _, queued := range signals {
		if queued == signal {
			return true
		}
	}
	return false
}

func (p *pendingSignals) clear(tid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.currentByTID, tid)
	delete(p.backlogByTID, tid)
	delete(p.delayedByTID, tid)
}

func (p *pendingSignals) purge() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.currentByTID = nil
	p.backlogByTID = nil
	p.delayedByTID = nil
}
