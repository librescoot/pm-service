package power

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// InhibitReason represents a reason for inhibiting power state changes
type InhibitReason string

const (
	InhibitReasonDownloading InhibitReason = "downloading"
	InhibitReasonInstalling  InhibitReason = "installing"
)

// InhibitRequest represents a request to inhibit power state changes
type InhibitRequest struct {
	ID       string        // Unique identifier for the inhibit request
	Reason   InhibitReason // Reason for inhibiting
	Duration time.Duration // Maximum duration for the inhibit (0 for indefinite)
	Defer    bool          // If true, defer power state changes completely; if false, delay them
}

// Inhibitor manages power state change inhibits
type Inhibitor struct {
	logger    *log.Logger
	mutex     sync.RWMutex
	inhibits  map[string]InhibitRequest
	timers    map[string]*time.Timer
	callbacks map[string]func()
}

// NewInhibitor creates a new power inhibitor
func NewInhibitor(logger *log.Logger) *Inhibitor {
	return &Inhibitor{
		logger:    logger,
		inhibits:  make(map[string]InhibitRequest),
		timers:    make(map[string]*time.Timer),
		callbacks: make(map[string]func()),
	}
}

// AddInhibit adds a new inhibit request
func (i *Inhibitor) AddInhibit(req InhibitRequest) error {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	if req.ID == "" {
		return fmt.Errorf("inhibit ID cannot be empty")
	}

	i.logger.Printf("Adding power inhibit: id=%s, reason=%s, duration=%v, defer=%v",
		req.ID, req.Reason, req.Duration, req.Defer)

	// Store the inhibit request
	i.inhibits[req.ID] = req

	// If duration is specified, create a timer to remove the inhibit
	if req.Duration > 0 {
		// Cancel existing timer if any
		if timer, exists := i.timers[req.ID]; exists {
			timer.Stop()
		}

		// Create a new timer
		i.timers[req.ID] = time.AfterFunc(req.Duration, func() {
			go i.RemoveInhibit(req.ID)
		})
	}

	return nil
}

// RemoveInhibit removes an inhibit request
func (i *Inhibitor) RemoveInhibit(id string) error {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	i.logger.Printf("Removing power inhibit: id=%s", id)

	// Check if the inhibit exists
	if _, exists := i.inhibits[id]; !exists {
		return fmt.Errorf("inhibit with ID %s does not exist", id)
	}

	// Stop the timer if it exists
	if timer, exists := i.timers[id]; exists {
		timer.Stop()
		delete(i.timers, id)
	}

	// Remove the inhibit
	delete(i.inhibits, id)

	// Execute callback if registered
	if callback, exists := i.callbacks[id]; exists {
		go callback()
		delete(i.callbacks, id)
	}

	return nil
}

// RegisterCallback registers a callback to be executed when an inhibit is removed
func (i *Inhibitor) RegisterCallback(id string, callback func()) {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	i.callbacks[id] = callback
}

// CanChangePowerState checks if power state changes are allowed
func (i *Inhibitor) CanChangePowerState() (bool, time.Duration, bool) {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	// If there are no inhibits, power state changes are allowed
	if len(i.inhibits) == 0 {
		return true, 0, false
	}

	// Check if any inhibit defers power state changes completely
	for _, req := range i.inhibits {
		if req.Defer {
			i.logger.Printf("Power state change deferred due to inhibit: %s (%s)", req.ID, req.Reason)
			return false, 0, true
		}
	}

	// If we have downloading inhibits, delay power state changes
	for _, req := range i.inhibits {
		if req.Reason == InhibitReasonDownloading {
			i.logger.Printf("Power state change delayed due to inhibit: %s (%s)", req.ID, req.Reason)
			return false, 5 * time.Minute, false
		}
	}

	// If we reach here, power state changes are allowed
	return true, 0, false
}

// HasInhibit checks if a specific inhibit exists
func (i *Inhibitor) HasInhibit(id string) bool {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	_, exists := i.inhibits[id]
	return exists
}

// GetInhibits returns a copy of all current inhibits
func (i *Inhibitor) GetInhibits() map[string]InhibitRequest {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	// Create a copy of the inhibits map
	inhibits := make(map[string]InhibitRequest, len(i.inhibits))
	for id, req := range i.inhibits {
		inhibits[id] = req
	}

	return inhibits
}
