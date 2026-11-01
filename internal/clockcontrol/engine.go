package clockcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const episodeWindow = 90 * time.Second
const sampleMaxAge = 30 * time.Second
const commandTimeout = 3 * time.Second

type state struct {
	BootID              string `json:"boot_id"`
	Generation          uint64 `json:"generation"`
	Episode             string `json:"episode"`
	Deadline            int64  `json:"deadline_boot_ns"`
	Outcome             string `json:"outcome"`
	Attempts            int    `json:"attempts"`
	LateUsed            bool   `json:"late_used"`
	LateHoldoverUsed    bool   `json:"late_holdover_used"`
	ExternalEstablished bool   `json:"external_established"`
	ExternalAfterBoot   int64  `json:"external_after_boot_ns"`
	Preparing           bool   `json:"preparing"`
	PrepareUTC          int64  `json:"prepare_utc_ns"`
	PrepareBoot         int64  `json:"prepare_boot_ns"`
	DispatchBoot        int64  `json:"dispatch_boot_ns"`
}
type sample struct {
	receipt, age int64
	uncertainty  int64
}
type Engine struct {
	mu          sync.Mutex
	clock       Clock
	chrony      Chrony
	ref         Refclock
	path        string
	state       state
	samples     map[string]sample
	quality     Quality
	qualityBoot time.Duration
	inFlight    chan struct{}
}

func persist(path string, s state) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".state-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func NewEngine(path, boot string, clock Clock, chrony Chrony, ref Refclock) (*Engine, error) {
	if boot == "" {
		return nil, errors.New("empty boot ID")
	}
	_, bt, err := clock.Now()
	if err != nil {
		return nil, err
	}
	e := &Engine{path: path, clock: clock, chrony: chrony, ref: ref, samples: make(map[string]sample), quality: Quality{Source: "unknown"}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		e.state = state{BootID: boot, Generation: 1, Episode: "initial", Deadline: int64(bt + episodeWindow), Outcome: "available"}
	case err != nil:
		return nil, err
	default:
		if err = json.Unmarshal(b, &e.state); err != nil {
			return nil, fmt.Errorf("invalid state: %w", err)
		}
		if e.state.BootID != boot {
			e.state = state{BootID: boot, Generation: 1, Episode: "initial", Deadline: int64(bt + episodeWindow), Outcome: "available"}
		} else if e.state.Generation == 0 || e.state.Outcome == "" {
			return nil, errors.New("invalid persisted state")
		} else if e.state.Outcome == "in-flight" {
			e.state.Outcome = "unknown"
		}
	}
	if err = persist(path, e.state); err != nil {
		return nil, err
	}
	return e, nil
}
func (e *Engine) save() error { return persist(e.path, e.state) }
func (e *Engine) statusLocked() Quality {
	q := e.quality
	_, bt, err := e.clock.Now()
	if err != nil || bt < e.qualityBoot {
		q = Quality{Source: "unknown"}
	} else if q.Source != "" && q.Source != "unknown" {
		q.AgeNS += int64(bt - e.qualityBoot)
		if q.AgeNS > int64(sampleMaxAge) {
			q.Source = "unknown"
			q.External = false
		}
	}
	q.BootID = e.state.BootID
	q.Generation = e.state.Generation
	q.Episode = e.state.Episode
	q.Outcome = e.state.Outcome
	q.Preparing = e.state.Preparing
	q.NRFDeadlineBootNS = e.state.Deadline
	q.NRFEligible = err == nil && bt >= e.qualityBoot && !e.state.Preparing && !e.state.ExternalEstablished &&
		(e.state.Episode == "initial" || e.state.Episode == "resume") &&
		e.state.Outcome == "available" && int64(bt) < e.state.Deadline &&
		!(q.External && q.AgeNS <= int64(sampleMaxAge) && math.Abs(q.ResidualSeconds)+q.UncertaintySeconds <= 5)
	return q
}
func (e *Engine) Status() Quality { e.mu.Lock(); defer e.mu.Unlock(); return e.statusLocked() }
func (e *Engine) Submit(r Request) error {
	var observed Tracking
	var observedBoot time.Duration
	var observedOK bool
	if r.Source == SourceNRF {
		now, bt, err := e.clock.Now()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		observed, err = e.chrony.Tracking(ctx, now)
		cancel()
		observedBoot, observedOK = bt, err == nil && observed.Valid
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now, bt, err := e.clock.Now()
	if err != nil {
		return err
	}
	if r.Source == SourceNRF && observedOK && bt >= observedBoot {
		observed.Age += bt - observedBoot
		external := observed.Source == "ntp" && observed.External
		if observed.Source == SourceGPS {
			sample, ok := e.samples[SourceGPS]
			if ok && int64(bt) >= sample.receipt && int64(bt)-sample.receipt <= int64(sampleMaxAge)-sample.age {
				age := time.Duration(int64(bt) - sample.receipt + sample.age)
				if observed.Age <= age+2*time.Second {
					if age > observed.Age {
						observed.Age = age
					}
					observed.Uncertainty += float64(sample.uncertainty) / float64(time.Second)
					external = true
				}
			}
		}
		if external && e.externalObservationFresh(observed, bt) && observed.Age <= sampleMaxAge && math.Abs(observed.Residual)+observed.Uncertainty <= 5 {
			e.state.ExternalEstablished = true
			e.state.ExternalAfterBoot = 0
			if err = e.save(); err != nil {
				return err
			}
			return errors.New("external time already confirmed")
		}
	}
	if r.BootID != e.state.BootID || r.Generation != e.state.Generation {
		return errors.New("stale boot or generation")
	}
	if e.state.Preparing {
		return errors.New("suspend preparation active")
	}
	if r.Source != SourceNRF && r.Source != SourceGPS && r.Source != SourceUser {
		return errors.New("unknown source")
	}
	q := e.statusLocked()
	now, bt, err = e.clock.Now()
	if err != nil {
		return err
	}
	if r.Source == SourceNRF && (!q.NRFEligible || int64(bt) >= e.state.Deadline) {
		return errors.New("nrf recovery unavailable")
	}
	if r.DeadlineBootNS != 0 && (r.DeadlineBootNS <= r.ReceiptBootNS || int64(bt) >= r.DeadlineBootNS) {
		return errors.New("sample deadline expired")
	}
	maxUncertainty := time.Second
	if r.Source == SourceGPS {
		maxUncertainty = 3 * time.Second
	}
	if r.ReceiptBootNS < 0 || r.ReceiptBootNS > int64(bt) || r.SampleAgeNS < 0 || r.UncertaintyNS < 0 || r.UncertaintyNS > int64(maxUncertainty) {
		return errors.New("invalid sample metadata")
	}
	age := int64(bt) - r.ReceiptBootNS
	if age > int64(sampleMaxAge) || r.SampleAgeNS > int64(sampleMaxAge)-age {
		return errors.New("stale sample")
	}
	age += r.SampleAgeNS
	if r.UTCUnixNS <= 0 || r.UTCUnixNS > math.MaxInt64-age {
		return errors.New("invalid sample UTC")
	}
	offset := time.Unix(0, r.UTCUnixNS+age).Sub(now).Seconds()
	if r.Source == SourceNRF && offset <= 0 {
		return errors.New("nrf sample would not advance clock")
	}
	if r.Source == SourceGPS && q.Source == "ntp" && q.External && q.AgeNS >= 0 && q.AgeNS <= int64(sampleMaxAge) && q.UncertaintySeconds >= 0 && math.Abs(q.ResidualSeconds)+q.UncertaintySeconds <= 5 && math.Abs(offset) > math.Abs(q.ResidualSeconds)+q.UncertaintySeconds+float64(r.UncertaintyNS)/float64(time.Second) {
		return errors.New("GPS sample disagrees with trusted NTP")
	}
	if math.Abs(offset) > float64(100*365*24*time.Hour/time.Second) {
		return errors.New("implausible sample offset")
	}
	if err = e.ref.Send(r.Source, now, offset); err != nil {
		return err
	}
	e.samples[r.Source] = sample{r.ReceiptBootNS, r.SampleAgeNS, r.UncertaintyNS}
	return nil
}

// After a clock-loss resume, tracking age must place the measurement after
// resume, rather than merely appearing fresh against the frozen wall clock.
func (e *Engine) externalObservationFresh(t Tracking, bt time.Duration) bool {
	if e.state.ExternalAfterBoot == 0 {
		return true
	}
	if int64(bt) <= e.state.ExternalAfterBoot {
		return false
	}
	if t.Source == SourceGPS {
		sample, ok := e.samples[SourceGPS]
		return ok && sample.receipt > e.state.ExternalAfterBoot
	}
	return t.Source == "ntp" && t.Age >= 0 && t.Age < time.Duration(int64(bt)-e.state.ExternalAfterBoot)-5*time.Second
}
func (e *Engine) refresh(ctx context.Context) {
	e.mu.Lock()
	generation := e.state.Generation
	e.mu.Unlock()
	now, startBoot, err := e.clock.Now()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	t, err := e.chrony.Tracking(ctx, now)
	e.mu.Lock()
	defer e.mu.Unlock()
	now, bt, clockErr := e.clock.Now()
	_ = now
	if clockErr != nil || bt < startBoot || generation != e.state.Generation {
		return
	}
	e.qualityBoot = bt
	if err == nil {
		t.Age += bt - startBoot
	}
	if err != nil || !t.Valid {
		e.quality = Quality{Source: "unknown"}
		return
	}
	if t.Source == SourceGPS || t.Source == SourceNRF || t.Source == SourceUser {
		s, ok := e.samples[t.Source]
		if !ok || int64(bt) < s.receipt || int64(bt)-s.receipt > int64(sampleMaxAge)-s.age {
			e.quality = Quality{Source: "unknown"}
			return
		}
		age := int64(bt) - s.receipt + s.age
		if t.Age > time.Duration(age)+2*time.Second {
			e.quality = Quality{Source: "unknown"}
			return
		}
		t.External = t.Source == SourceGPS
		if time.Duration(age) > t.Age {
			t.Age = time.Duration(age)
		}
		t.Uncertainty += float64(s.uncertainty) / float64(time.Second)
	}
	if t.External && !e.externalObservationFresh(t, bt) {
		e.quality = Quality{Source: "unknown"}
		return
	}
	if t.External && t.Age <= sampleMaxAge && math.Abs(t.Residual)+t.Uncertainty <= 5 && !e.state.ExternalEstablished {
		e.state.ExternalEstablished = true
		e.state.ExternalAfterBoot = 0
		if e.save() != nil {
			e.quality = Quality{Source: "unknown"}
			return
		}
	}
	e.quality = Quality{Source: t.Source, External: t.External, AgeNS: int64(t.Age), ResidualSeconds: t.Residual, UncertaintySeconds: t.Uncertainty}
	if e.state.Outcome == "unknown" && t.Source != "local" && int64(bt)-e.state.DispatchBoot >= int64(10*time.Second) && t.Age < 5*time.Second && math.Abs(t.Residual) < 0.1 && t.Uncertainty < 5 {
		e.state.Outcome = "reconciled"
		if e.save() != nil {
			e.state.Outcome = "unknown"
			e.quality = Quality{Source: "unknown"}
			return
		}
	}
	if e.state.Outcome == "unknown" || e.state.Outcome == "in-flight" {
		e.quality = Quality{Source: "unknown"}
		return
	}
	if e.state.Preparing {
		return
	}
	lateExternal := t.External && !e.state.LateUsed
	lateHoldover := t.Source == SourceNetworkHoldover && !e.state.LateHoldoverUsed && !e.state.ExternalEstablished
	if (lateExternal || lateHoldover) && t.Age <= sampleMaxAge && t.Uncertainty <= 5 && math.Abs(t.Residual) > 1 && int64(bt) >= e.state.Deadline {
		old := e.state
		if lateExternal {
			e.state.LateUsed = true
			e.state.Episode = "late-external"
		} else {
			e.state.LateHoldoverUsed = true
			e.state.Episode = "late-holdover"
		}
		e.state.Deadline = int64(bt + episodeWindow)
		e.state.Outcome = "available"
		e.state.Attempts = 0
		if e.save() != nil {
			e.state = old
			return
		}
	}
	if (t.Source != "ntp" && t.Source != SourceNRF && t.Source != SourceGPS && t.Source != SourceUser && t.Source != SourceNetworkHoldover) || (t.Source == SourceNetworkHoldover && e.state.ExternalEstablished) || e.state.Outcome != "available" || int64(bt) >= e.state.Deadline || t.Age > sampleMaxAge || t.Uncertainty > 5 || math.Abs(t.Residual) < 1 {
		return
	}
	old := e.state
	admissionBoot := bt
	e.state.Outcome = "in-flight"
	e.quality = Quality{Source: "unknown"}
	e.state.Attempts++
	e.state.DispatchBoot = int64(bt)
	if e.save() != nil {
		e.state = old
		return
	}
	_, bt, err = e.clock.Now()
	if err != nil || bt < admissionBoot || int64(bt) >= e.state.Deadline || t.Age+(bt-admissionBoot) > sampleMaxAge {
		e.state.Outcome = "expired"
		_ = e.save()
		return
	}
	cmdctx, cancelStep := context.WithTimeout(context.Background(), commandTimeout)
	done, err := e.chrony.StartStep(cmdctx)
	if err != nil {
		cancelStep()
		if e.state.Attempts < 2 {
			e.state.Outcome = "available"
		} else {
			e.state.Outcome = "failed"
		}
		if e.save() != nil {
			e.state.Outcome = "unknown"
		}
		return
	}
	e.inFlight = make(chan struct{})
	finished := e.inFlight
	go func() {
		defer cancelStep()
		var err error
		select {
		case err = <-done:
		case <-cmdctx.Done():
			err = cmdctx.Err()
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if err == nil {
			e.state.Outcome = "completed"
		} else {
			e.state.Outcome = "unknown"
		}
		if e.save() != nil {
			e.state.Outcome = "unknown"
		}
		close(finished)
		e.inFlight = nil
	}()
}
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		e.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (e *Engine) Prepare(ctx context.Context) error {
	e.mu.Lock()
	if e.state.Preparing {
		e.mu.Unlock()
		return errors.New("already preparing")
	}
	now, bt, err := e.clock.Now()
	if err != nil {
		e.mu.Unlock()
		return err
	}
	e.state.Preparing = true
	e.state.PrepareUTC = now.UnixNano()
	e.state.PrepareBoot = int64(bt)
	if err = e.save(); err != nil {
		e.state.Preparing = false
		e.mu.Unlock()
		return err
	}
	ch := e.inFlight
	e.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			e.mu.Lock()
			e.state.Preparing = false
			err = e.save()
			e.mu.Unlock()
			if err != nil {
				return err
			}
			return ctx.Err()
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Outcome == "unknown" || e.state.Outcome == "in-flight" {
		e.state.Preparing = false
		if err = e.save(); err != nil {
			return err
		}
		return errors.New("step outcome unresolved")
	}
	return nil
}
func (e *Engine) Abort() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.state.Preparing {
		return errors.New("not preparing")
	}
	e.state.Preparing = false
	if err := e.save(); err != nil {
		e.state.Preparing = true
		return err
	}
	return nil
}
func (e *Engine) Resume(boot string, generation uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.state.Preparing || boot != e.state.BootID || generation != e.state.Generation {
		return errors.New("invalid resume generation")
	}
	now, bt, err := e.clock.Now()
	if err != nil {
		return err
	}
	if int64(bt) < e.state.PrepareBoot {
		return errors.New("boottime went backwards")
	}
	loss := int64(bt) - e.state.PrepareBoot - (now.UnixNano() - e.state.PrepareUTC)
	old := e.state
	e.state.Preparing = false
	e.state.Generation++
	if loss > int64(2*time.Second) {
		e.state.ExternalEstablished = false
		e.state.ExternalAfterBoot = int64(bt)
		e.state.Episode = "resume"
		e.state.Deadline = int64(bt + episodeWindow)
		e.state.Outcome = "available"
		e.state.Attempts = 0
	}
	if err = e.save(); err != nil {
		e.state = old
		return err
	}
	e.samples = make(map[string]sample)
	e.quality = Quality{Source: "unknown"}
	return nil
}
