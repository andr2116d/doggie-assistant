package claudecode

import "time"

// maxEpochs is how many processes are kept for the next turns of their
// threads. Each takes about 200 MB of memory.
const maxEpochs = 2

// An epoch is a process kept after a turn of a thread, holding the thread's
// conversation, to serve its next turns: they send only what is new, and
// the CLI reads the rest back from the prompt cache instead of writing it
// again. The provider's epochs are the ones waiting for a turn; while one
// serves a turn, the turn owns it.

// takeEpoch returns the process kept for thread, if it can serve a turn
// now. Like take, the caller owns a wg slot until the turn ends.
func (p *Provider) takeEpoch(thread string) *proc {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.epochs[thread]
	if pr == nil || p.closed {
		return nil
	}
	delete(p.epochs, thread)
	if !pr.alive() || pr.backup != p.backupLocked() {
		p.discardLater(pr)
		return nil
	}
	p.wg.Add(1)
	return pr
}

// keepEpoch keeps pr for the next turn of thread, and reports false if the
// provider is closed. Past maxEpochs, the one used longest ago is stopped.
func (p *Provider) keepEpoch(thread string, pr *proc) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	pr.used = time.Now()
	if old := p.epochs[thread]; old != nil {
		p.discardLater(old)
	}
	p.epochs[thread] = pr
	for len(p.epochs) > maxEpochs {
		oldest := thread
		for t, e := range p.epochs {
			if e.used.Before(p.epochs[oldest].used) {
				oldest = t
			}
		}
		p.discardLater(p.epochs[oldest])
		delete(p.epochs, oldest)
	}
	return true
}

// discardLater stops pr apart, since stopping waits for it to exit. The
// caller holds p.mu and the provider isn't closed.
func (p *Provider) discardLater(pr *proc) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.discard(pr)
	}()
}
