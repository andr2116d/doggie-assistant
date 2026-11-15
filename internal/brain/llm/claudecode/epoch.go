package claudecode

import (
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
)

// maxEpochs is how many processes are kept for the next turns of their
// threads. Each takes about 200 MB of memory.
const maxEpochs = 2

// maxEpochTokens is how large a conversation a kept process may hold. Past
// it the thread starts over on a new process, with the context DOGGIE
// builds, well before the CLI would shorten the conversation on its own.
// A variable so tests can shrink it.
var maxEpochTokens = 60_000

// fits reports whether the conversation of a turn that used t is small
// enough to keep its process. The input of all the turn's calls to the
// model is counted, which is more than the conversation when the turn
// called tools; an unknown size doesn't fit.
func fits(t *llm.Tokens) bool {
	return t != nil && t.Input+t.CacheRead+t.CacheWrite <= maxEpochTokens
}

// epochIdle is how long a kept process waits for its thread's next turn.
// The prompt cache forgets the conversation after an hour, and from then
// on a kept process saves nothing. A variable so tests can shrink it.
var epochIdle = 55 * time.Minute

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
	if p.epochs[thread] == nil || p.closed {
		return nil
	}
	pr := p.forgetEpoch(thread)
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
	if p.epochs[thread] != nil {
		p.discardLater(p.forgetEpoch(thread))
	}
	pr.used, pr.released = time.Now(), make(chan struct{})
	p.epochs[thread] = pr
	p.wg.Add(1)
	go p.watchEpoch(thread, pr, pr.released)
	for len(p.epochs) > maxEpochs {
		oldest := thread
		for t, e := range p.epochs {
			if e.used.Before(p.epochs[oldest].used) {
				oldest = t
			}
		}
		p.discardLater(p.forgetEpoch(oldest))
	}
	return true
}

// forgetEpoch takes the process kept for thread out of the epochs, which
// ends its watcher, and returns it. The caller holds p.mu.
func (p *Provider) forgetEpoch(thread string) *proc {
	pr := p.epochs[thread]
	delete(p.epochs, thread)
	close(pr.released)
	return pr
}

// watchEpoch stops the process kept for thread if it dies or waits too
// long. It ends once the process leaves the epochs: released is closed.
func (p *Provider) watchEpoch(thread string, pr *proc, released <-chan struct{}) {
	defer p.wg.Done()
	timer := time.NewTimer(epochIdle)
	defer timer.Stop()
	select {
	case <-released:
		return
	case <-p.done:
		return
	case <-pr.exited:
	case <-timer.C:
	}
	p.mu.Lock()
	if p.closed || p.epochs[thread] != pr {
		p.mu.Unlock()
		return
	}
	p.forgetEpoch(thread)
	p.mu.Unlock()
	p.discard(pr)
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
