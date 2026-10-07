package service

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/librescoot/librefsm"
	"github.com/librescoot/pm-service/internal/fsm"
	redis_ipc "github.com/librescoot/redis-ipc"
)

const hibernatePreparationTimeout = 30 * time.Second

func (s *Service) waitForWakeTimer(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var done <-chan struct{}
	if s.ctx != nil {
		done = s.ctx.Done()
	}
	for {
		select {
		case seconds := <-s.wakeTimerAcks:
			if seconds > 0 && seconds == s.fsmData.HibernateForWakeSeconds {
				return true
			}
			s.logger.Printf("Ignoring wake timer ACK for %d seconds (expecting %d)", seconds, s.fsmData.HibernateForWakeSeconds)
		case <-timer.C:
			return false
		case <-done:
			return false
		}
	}
}

func (s *Service) explicitHibernateState(target string) (string, error) {
	state, err := s.redis.HGet("vehicle", "state")
	if err != nil {
		return "", fmt.Errorf("cannot read vehicle state: %w", err)
	}
	if state != "stand-by" && state != "parked" {
		return "", fmt.Errorf("vehicle must be parked or in stand-by (is %s)", state)
	}
	// Vehicle preparation can interrupt dashboard work. Real block inhibitors
	// must be respected before that preparation, not just before MDB poweroff.
	if state == "parked" && s.inhibitorManager.HasBlockingInhibitors(target) && !s.hasOnlyModemBlockingInhibitors(target) {
		return "", fmt.Errorf("vehicle preparation blocked by power inhibitor")
	}
	return state, nil
}

func (s *Service) CanAcceptExplicitHibernate(c *librefsm.Context) bool {
	_, err := s.explicitHibernateState(s.targetPowerStateFromContext(c))
	if err != nil {
		s.logger.Printf("Hibernation request rejected: %v", err)
		s.setHibernateStatus("rejected", err.Error())
		return false
	}
	return true
}

func (s *Service) setHibernateStatus(status, reason string) {
	if s.powerManagerPub == nil {
		return
	}
	if err := s.powerManagerPub.SetMany(map[string]any{
		"hibernate-status": status,
		"hibernate-error":  reason,
	}, redis_ipc.Sync()); err != nil {
		s.logger.Printf("Failed to publish hibernation status: %v", err)
	}
}

func (s *Service) stopHibernatePreparationTimer() {
	if s.hibernatePreparationTimer != nil {
		s.hibernatePreparationTimer.Stop()
		s.hibernatePreparationTimer = nil
	}
}

func (s *Service) clearHibernateRequest(status, reason string) {
	s.stopHibernatePreparationTimer()
	if s.fsmData.HibernateRequestID != "" {
		s.fsmData.HibernateRequestID = ""
		if s.powerManagerPub != nil {
			if err := s.powerManagerPub.Set("hibernate-request-id", "", redis_ipc.Sync()); err != nil {
				s.logger.Printf("Failed to invalidate hibernation preparation: %v", err)
			}
		}
		s.setHibernateStatus(status, reason)
	}
	if s.fsmData.HibernateForWakeSeconds > 0 || s.wakeTimerArmed {
		if s.powerManagerPub != nil {
			if err := s.powerManagerPub.Set("wake-timer-seconds", "0", redis_ipc.Sync()); err != nil {
				s.logger.Printf("Failed to disarm wake timer: %v", err)
			}
		}
	}
	s.fsmData.HibernateForWakeSeconds = 0
	s.wakeTimerArmed = false
}

func (s *Service) prepareExplicitHibernate(state string) error {
	id := rand.Text()
	s.fsmData.HibernateRequestID = id
	status := "preparing-power"
	if state == "parked" {
		status = "preparing-vehicle"
	}
	if err := s.powerManagerPub.SetMany(map[string]any{
		"hibernate-request-id": id,
		"hibernate-status":     status,
		"hibernate-error":      "",
	}, redis_ipc.Sync()); err != nil {
		return err
	}
	if state == "stand-by" {
		return nil
	}
	if _, err := s.redis.LPush("scooter:state", "prepare-hibernate:"+id); err != nil {
		return err
	}
	s.hibernatePreparationTimer = time.AfterFunc(hibernatePreparationTimeout, func() {
		s.machine.Send(librefsm.Event{
			ID:      fsm.EvHibernatePreparationFailed,
			Payload: fsm.HibernatePreparationResult{RequestID: id, Error: "vehicle preparation timed out"},
		})
	})
	return nil
}

func (s *Service) onHibernatePreparationResult(value string) error {
	id, reason, ok := strings.Cut(value, ":")
	if ok && s.machine != nil {
		s.machine.Send(librefsm.Event{
			ID:      fsm.EvHibernatePreparationFailed,
			Payload: fsm.HibernatePreparationResult{RequestID: id, Error: reason},
		})
	}
	return nil
}

func (s *Service) OnHibernatePreparationFailed(c *librefsm.Context) error {
	p, ok := c.Event.Payload.(fsm.HibernatePreparationResult)
	if !ok || p.RequestID == "" || p.RequestID != s.fsmData.HibernateRequestID {
		return nil
	}
	s.logger.Printf("Hibernation preparation failed: %s", p.Error)
	s.clearHibernateRequest("failed", p.Error)
	// Failed explicit requests must not turn into a later default hibernation.
	s.fsmData.TargetPowerState = fsm.TargetRun
	return nil
}
