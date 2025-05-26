package hibernation

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// HibernationState represents the states in the manual hibernation sequence
type HibernationState int

const (
	HibernationStateIdle HibernationState = iota
	HibernationStateWaiting
	HibernationStateAdvanced
	HibernationStateSeatbox
	HibernationStateConfirm
)

// String returns the string representation of the hibernation state
func (h HibernationState) String() string {
	switch h {
	case HibernationStateIdle:
		return "idle"
	case HibernationStateWaiting:
		return "waiting-hibernation"
	case HibernationStateAdvanced:
		return "waiting-hibernation-advanced"
	case HibernationStateSeatbox:
		return "waiting-hibernation-seatbox"
	case HibernationStateConfirm:
		return "waiting-hibernation-confirm"
	default:
		return "unknown"
	}
}

// Timing constants for hibernation sequence
const (
	HibernationStartTime   = 15 * time.Second
	HibernationExitTime    = 60 * time.Second
	HibernationAdvanceTime = 10 * time.Second
	HibernationConfirmTime = 3 * time.Second
)

// StateMachine manages the manual hibernation sequence
type StateMachine struct {
	state  HibernationState
	logger *log.Logger
	redis  *redis.Client
	ctx    context.Context

	timer      *time.Timer
	onComplete func() // Callback when hibernation sequence completes
}

// NewStateMachine creates a new hibernation state machine
func NewStateMachine(ctx context.Context, redis *redis.Client, logger *log.Logger, onComplete func()) *StateMachine {
	return &StateMachine{
		state:      HibernationStateIdle,
		logger:     logger,
		redis:      redis,
		ctx:        ctx,
		onComplete: onComplete,
	}
}

// GetState returns the current hibernation state
func (sm *StateMachine) GetState() HibernationState {
	return sm.state
}

// StartHibernationSequence begins the manual hibernation process
func (sm *StateMachine) StartHibernationSequence() {
	if sm.state != HibernationStateIdle {
		sm.logger.Printf("Hibernation sequence already in progress, current state: %s", sm.state)
		return
	}

	sm.logger.Printf("Starting manual hibernation sequence")
	sm.setState(HibernationStateWaiting)
	sm.startTimer(HibernationStartTime, sm.onStartTimer)
}

// ProcessInput handles input events during hibernation sequence
func (sm *StateMachine) ProcessInput(inputType, value string) {
	switch sm.state {
	case HibernationStateWaiting:
		// Check for hibernation input held
		if inputType == "hibernation_input" && value == "released" {
			sm.logger.Printf("Hibernation input released during waiting state, canceling sequence")
			sm.cancelSequence()
		}

	case HibernationStateAdvanced:
		// Check for continued hold
		if inputType == "hibernation_input" && value == "released" {
			sm.logger.Printf("Hibernation input released during advanced state, canceling sequence")
			sm.cancelSequence()
		}

	case HibernationStateSeatbox:
		// Check for seatbox closure
		if inputType == "seatbox" && value == "closed" {
			sm.logger.Printf("Seatbox closed, proceeding to confirm state")
			sm.setState(HibernationStateConfirm)
			sm.startTimer(HibernationConfirmTime, sm.onConfirmTimer)
		} else if inputType == "hibernation_input" && value == "released" {
			sm.logger.Printf("Hibernation input released during seatbox state, canceling sequence")
			sm.cancelSequence()
		}

	case HibernationStateConfirm:
		// Final confirmation
		if inputType == "hibernation_input" && value == "pressed" {
			sm.logger.Printf("Final hibernation confirmation received, executing hibernation")
			sm.executeHibernation()
		}
	}
}

// CancelSequence cancels the hibernation sequence
func (sm *StateMachine) CancelSequence() {
	sm.cancelSequence()
}

// setState changes the hibernation state and publishes to Redis
func (sm *StateMachine) setState(newState HibernationState) {
	if sm.state == newState {
		return
	}

	oldState := sm.state
	sm.state = newState

	sm.logger.Printf("Hibernation state transition: %s -> %s", oldState, newState)

	// Publish state change to Redis
	pipe := sm.redis.Pipeline()
	pipe.HSet(sm.ctx, "hibernation", "state", newState.String())
	pipe.Publish(sm.ctx, "hibernation", "state")
	_, err := pipe.Exec(sm.ctx)
	if err != nil {
		sm.logger.Printf("Warning: Failed to publish hibernation state: %v", err)
	}

	// Stop any existing timer when changing states
	sm.stopTimer()
}

// startTimer starts a timer with the given duration and callback
func (sm *StateMachine) startTimer(duration time.Duration, callback func()) {
	sm.stopTimer()
	sm.timer = time.AfterFunc(duration, callback)
	sm.logger.Printf("Started hibernation timer for %v", duration)
}

// stopTimer stops the current timer
func (sm *StateMachine) stopTimer() {
	if sm.timer != nil {
		sm.timer.Stop()
		sm.timer = nil
	}
}

// onStartTimer handles the initial hibernation timer
func (sm *StateMachine) onStartTimer() {
	if sm.state != HibernationStateWaiting {
		return
	}

	sm.logger.Printf("Hibernation start timer elapsed, advancing to advanced state")
	sm.setState(HibernationStateAdvanced)
	sm.startTimer(HibernationAdvanceTime, sm.onAdvanceTimer)
}

// onAdvanceTimer handles the advance timer
func (sm *StateMachine) onAdvanceTimer() {
	if sm.state != HibernationStateAdvanced {
		return
	}

	sm.logger.Printf("Hibernation advance timer elapsed, waiting for seatbox closure")
	sm.setState(HibernationStateSeatbox)
	sm.startTimer(HibernationExitTime, sm.onExitTimer)
}

// onConfirmTimer handles the confirmation timer
func (sm *StateMachine) onConfirmTimer() {
	if sm.state != HibernationStateConfirm {
		return
	}

	sm.logger.Printf("Hibernation confirm timer elapsed, canceling sequence")
	sm.cancelSequence()
}

// onExitTimer handles the exit timer
func (sm *StateMachine) onExitTimer() {
	sm.logger.Printf("Hibernation exit timer elapsed, canceling sequence")
	sm.cancelSequence()
}

// cancelSequence resets the hibernation sequence
func (sm *StateMachine) cancelSequence() {
	sm.logger.Printf("Canceling hibernation sequence from state: %s", sm.state)
	sm.stopTimer()
	sm.setState(HibernationStateIdle)
}

// executeHibernation completes the hibernation sequence
func (sm *StateMachine) executeHibernation() {
	sm.logger.Printf("Executing manual hibernation")
	sm.stopTimer()
	sm.setState(HibernationStateIdle)

	if sm.onComplete != nil {
		sm.onComplete()
	}
}

// Close cleans up the state machine
func (sm *StateMachine) Close() {
	sm.stopTimer()
}
