package scheduler

import (
	"log"
	"sync"
	"time"

	"github.com/boltq/boltq/internal/broker"
)

// Scheduler runs background tasks: ACK timeout watcher and dead-letter handler.
type Scheduler struct {
	broker   *broker.Broker
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// New creates a new scheduler.
func New(b *broker.Broker, checkInterval time.Duration) *Scheduler {
	if checkInterval <= 0 {
		checkInterval = time.Second
	}
	return &Scheduler{
		broker:   b,
		interval: checkInterval,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the scheduler background goroutines.
func (s *Scheduler) Start() {
	s.wg.Add(1)
	go s.ackTimeoutWatcher()
}

// Stop gracefully stops all scheduler goroutines.
func (s *Scheduler) Stop() {
	close(s.stopCh)
	s.wg.Wait()
}

// ackTimeoutWatcher checks for messages that haven't been ACK'd within the timeout
// and requeues them (with retry/dead-letter logic).
func (s *Scheduler) ackTimeoutWatcher() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.checkTimeouts()
		}
	}
}

func (s *Scheduler) checkTimeouts() {
	now := time.Now()
	pending := s.broker.GetPendingMessages()

	for id, pm := range pending {
		if now.After(pm.AckDeadline) {
			if err := s.broker.RequeueTimedOut(id); err != nil {
				log.Printf("[scheduler] requeue timeout msg %s: %v", id, err)
			}
		}
	}
}
