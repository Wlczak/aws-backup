// Package scheduler runs the configured cron schedule, calling the engine
// trigger each time it fires. The trigger itself is responsible for
// deciding what to do when another run is already in progress (the API
// handler returns 409; the scheduler just invokes and logs).
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Trigger is the callback the scheduler invokes each time the cron fires.
// It returns an error so the scheduler can log failures; a nil return does
// not imply the run succeeded, only that it was dispatched.
type Trigger func(ctx context.Context) error

// Scheduler owns a robfig cron + one bound Trigger.
type Scheduler struct {
	mu        sync.Mutex
	schedule  string
	cron      *cron.Cron
	entryID   cron.EntryID
	trigger   Trigger
	logger    *slog.Logger
	triggered int64 // count of invocations, for tests / telemetry
}

// New returns a Scheduler wrapping cron.New(WithSeconds()=false). An
// empty schedule is allowed — the scheduler is idle until Update() is
// called with a valid expression.
func New(schedule string, trigger Trigger, logger *slog.Logger) (*Scheduler, error) {
	if trigger == nil {
		return nil, errors.New("scheduler: trigger is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Scheduler{
		schedule: schedule,
		cron:     cron.New(),
		trigger:  trigger,
		logger:   logger,
	}
	if schedule != "" {
		if err := s.setSchedule(schedule); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Start kicks off the cron loop. Safe to call once.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop blocks until the currently running job (if any) finishes, then
// stops the scheduler. Safe to call multiple times.
func (s *Scheduler) Stop() {
	ctx := s.cron.Stop()
	<-ctx.Done()
}

// Update replaces the active schedule with newSchedule (can be empty to
// disable). Returns an error if the expression is invalid.
func (s *Scheduler) Update(newSchedule string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entryID != 0 {
		s.cron.Remove(s.entryID)
		s.entryID = 0
	}
	s.schedule = newSchedule
	if newSchedule == "" {
		return nil
	}
	return s.setScheduleLocked(newSchedule)
}

// Next returns the next scheduled fire time (zero if idle / no schedule).
func (s *Scheduler) Next() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entryID == 0 {
		return time.Time{}
	}
	e := s.cron.Entry(s.entryID)
	return e.Next
}

// Schedule returns the current schedule string.
func (s *Scheduler) Schedule() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.schedule
}

// Triggered returns the total number of times the cron fired (for
// observability + tests).
func (s *Scheduler) Triggered() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.triggered
}

// setSchedule / setScheduleLocked are separated so Update can avoid
// double-locking.
func (s *Scheduler) setSchedule(expr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setScheduleLocked(expr)
}

func (s *Scheduler) setScheduleLocked(expr string) error {
	id, err := s.cron.AddFunc(expr, func() {
		s.mu.Lock()
		s.triggered++
		s.mu.Unlock()
		// Give the trigger a timeout so a misbehaving engine doesn't
		// starve the next tick.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.trigger(ctx); err != nil {
			s.logger.Warn("scheduler trigger failed", "error", err)
		}
	})
	if err != nil {
		return err
	}
	s.entryID = id
	return nil
}
