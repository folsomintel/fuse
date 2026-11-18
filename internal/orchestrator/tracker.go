package orchestrator

import "sync"

// tracker counts work in flight, like a sync.WaitGroup, except that Add may
// run while Wait is waiting. Stop waits on one while restate handlers and the
// api can still start background work, which a WaitGroup forbids.
type tracker struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

func (t *tracker) Add(delta int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n == 0 {
		t.idle = make(chan struct{})
	}
	t.n += delta
	if t.n == 0 {
		close(t.idle)
	}
}

func (t *tracker) Done() { t.Add(-1) }

// Wait returns once nothing is in flight. work started while it waits is
// waited for too.
func (t *tracker) Wait() {
	for {
		t.mu.Lock()
		if t.n == 0 {
			t.mu.Unlock()
			return
		}
		idle := t.idle
		t.mu.Unlock()
		<-idle
	}
}
