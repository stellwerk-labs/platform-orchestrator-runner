package rollout

import "errors"

var errWorkerUnavailable = errors.New("worker_not_accepting")

// WorkerStatus separates storage authority from readiness for new work. Delivery
// and operator status remain available after an ordinary irreversible drain.
type WorkerStatus struct {
	AuthorityAlive bool `json:"authority_alive"`
	Accepting      bool `json:"accepting"`
	Draining       bool `json:"draining"`
	ActiveStages   int  `json:"active_stages"`
}

func (e *Engine) WorkerStatus() WorkerStatus {
	e.stageMu.Lock()
	defer e.stageMu.Unlock()
	alive := e.Ready()
	return WorkerStatus{AuthorityAlive: alive, Accepting: alive && !e.draining, Draining: e.draining, ActiveStages: e.activeStages}
}

func (e *Engine) Accepting() bool { return e.WorkerStatus().Accepting }

func (e *Engine) checkAdmission() error {
	if e.stopped.Load() {
		return errStorageUnavailable
	}
	if e.closed.Load() {
		return errors.New("coordinator_closed")
	}
	if !e.Accepting() {
		return errWorkerUnavailable
	}
	return nil
}

// Drain closes stage admission, but never cancels a healthy execution. The host
// must explicitly stop the worker after ActiveStages reaches zero.
func (e *Engine) Drain() WorkerStatus {
	e.stageMu.Lock()
	e.draining = true
	e.stageMu.Unlock()
	return e.WorkerStatus()
}

func (e *Engine) admitStage() error {
	e.stageMu.Lock()
	if !e.draining && !e.stopped.Load() && !e.closed.Load() {
		e.activeStages++
		e.stageMu.Unlock()
		return nil
	}
	e.stageMu.Unlock()
	// Only public root Shutdown may unwind this wait. A step-local error or
	// cancellation would durably poison a queued workflow instead of parking it.
	<-e.durable.Done()
	return e.durable.Err()
}

func (e *Engine) releaseStage() {
	e.stageMu.Lock()
	e.activeStages--
	e.stageMu.Unlock()
}
