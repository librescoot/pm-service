package clockcontrol

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu   sync.Mutex
	utc  time.Time
	boot time.Duration
}

func (c *fakeClock) Now() (time.Time, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.utc, c.boot, nil
}
func (c *fakeClock) advance(wall, boot time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.utc = c.utc.Add(wall)
	c.boot += boot
}

type fakeChrony struct {
	mu       sync.Mutex
	t        Tracking
	started  int
	startErr error
	done     chan error
}

func (c *fakeChrony) Tracking(context.Context, time.Time) (Tracking, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t, nil
}
func (c *fakeChrony) StartStep(context.Context) (<-chan error, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started++
	if c.startErr != nil {
		return nil, c.startErr
	}
	return c.done, nil
}
func (c *fakeChrony) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.started }

type fakeRef struct {
	offset float64
	calls  int
}

func (r *fakeRef) Send(_ string, _ time.Time, v float64) error { r.offset = v; r.calls++; return nil }
func newTestEngine(t *testing.T) (*Engine, *fakeClock, *fakeChrony, *fakeRef, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state")
	clock := &fakeClock{utc: time.Unix(1700000000, 0), boot: 10 * time.Second}
	ch := &fakeChrony{t: Tracking{Source: "ntp", External: true, Age: time.Second, Residual: 10, Uncertainty: .1, Valid: true}, done: make(chan error, 2)}
	ref := &fakeRef{}
	e, err := NewEngine(p, "boot", clock, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	return e, clock, ch, ref, p
}
func waitOutcome(t *testing.T, e *Engine, out string) {
	t.Helper()
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		if e.Status().Outcome == out {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("outcome %s want %s", e.Status().Outcome, out)
}
func TestSOCKLayout(t *testing.T) {
	for _, tc := range []struct{ word, size, off int }{{4, 32, 8}, {8, 40, 16}} {
		b, err := EncodeSOCK(time.Unix(42, 123456000), -1.5, tc.word, binary.LittleEndian)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != tc.size || binary.LittleEndian.Uint32(b[0:4]) != 42 || math.Float64frombits(binary.LittleEndian.Uint64(b[tc.off:])) != -1.5 || binary.LittleEndian.Uint32(b[tc.off+20:]) != 0x534f434b {
			t.Fatalf("wrong %d-bit layout: %x", tc.word*8, b)
		}
		for _, v := range b[tc.off+8 : tc.off+20] {
			if v != 0 {
				t.Fatal("nonzero flags")
			}
		}
	}
}
func TestSubmitAgeGenerationAndSources(t *testing.T) {
	e, c, _, ref, _ := newTestEngine(t)
	now, boot, _ := c.Now()
	r := Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.Add(5 * time.Second).UnixNano(), ReceiptBootNS: int64(boot - 2*time.Second), SampleAgeNS: int64(time.Second)}
	if err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	if ref.offset != 8 {
		t.Fatalf("offset=%f", ref.offset)
	}
	r.Generation = 2
	if e.Submit(r) == nil {
		t.Fatal("old generation accepted")
	}
	r.Generation = 1
	r.SampleAgeNS = int64(time.Minute)
	if e.Submit(r) == nil {
		t.Fatal("stale sample accepted")
	}
	if ref.calls != 1 {
		t.Fatal(ref.calls)
	}
}
func TestNRFEligibilityExpiresAcrossSuspendAndRestart(t *testing.T) {
	e, c, ch, ref, path := newTestEngine(t)
	if !e.Status().NRFEligible {
		t.Fatal("initial recovery should be eligible")
	}
	c.advance(0, episodeWindow)
	if e.Status().NRFEligible {
		t.Fatal("suspend must count toward recovery deadline")
	}
	now, boot, _ := c.Now()
	r := Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.UnixNano(), ReceiptBootNS: int64(boot)}
	if e.Submit(r) == nil || ref.calls != 0 {
		t.Fatal("expired recovery sample accepted")
	}
	restarted, err := NewEngine(path, "boot", c, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Status().NRFEligible || restarted.Submit(r) == nil {
		t.Fatal("restart reopened recovery")
	}
}

func TestStepPersistRestartAndDeadline(t *testing.T) {
	e, c, ch, ref, p := newTestEngine(t)
	e.refresh(context.Background())
	if ch.count() != 1 || e.Status().Outcome != "in-flight" {
		t.Fatal(e.Status())
	}
	restarted, err := NewEngine(p, "boot", c, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Status().Outcome != "unknown" {
		t.Fatal(restarted.Status())
	}
	restarted.refresh(context.Background())
	if ch.count() != 1 {
		t.Fatal("replayed step")
	}
	ch.done <- nil
	waitOutcome(t, e, "completed")
	e2, c2, ch2, _, _ := newTestEngine(t)
	c2.advance(91*time.Second, 91*time.Second)
	ch2.t.External = false
	e2.refresh(context.Background())
	if ch2.count() != 0 {
		t.Fatal("dispatched after deadline")
	}
}
func TestPredispatchRetryBound(t *testing.T) {
	e, _, ch, _, _ := newTestEngine(t)
	ch.startErr = errors.New("missing executable")
	e.refresh(context.Background())
	if e.Status().Outcome != "available" {
		t.Fatal(e.Status())
	}
	e.refresh(context.Background())
	e.refresh(context.Background())
	if ch.count() != 2 || e.Status().Outcome != "failed" {
		t.Fatal(ch.count(), e.Status())
	}
}
func TestUnknownPrepareAndReconciliation(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	e.refresh(context.Background())
	ch.done <- errors.New("response lost")
	waitOutcome(t, e, "unknown")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e.Prepare(ctx) == nil || e.Status().Preparing {
		t.Fatal("unknown allowed suspend")
	}
	c.advance(11*time.Second, 11*time.Second)
	ch.mu.Lock()
	ch.t.Residual = .01
	ch.mu.Unlock()
	e.refresh(context.Background())
	if e.Status().Outcome != "reconciled" {
		t.Fatal(e.Status())
	}
}
func TestPrepareWaitAndResume(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	e.refresh(context.Background())
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result <- e.Prepare(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	if !e.Status().Preparing {
		t.Fatal("prepare did not fence")
	}
	e.refresh(context.Background())
	if ch.count() != 1 {
		t.Fatal("step during prepare")
	}
	ch.done <- nil
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	c.advance(time.Second, 11*time.Second)
	if e.Resume("boot", 1) != nil {
		t.Fatal("resume failed")
	}
	q := e.Status()
	if q.Generation != 2 || q.Episode != "resume" || q.Outcome != "available" {
		t.Fatal(q)
	}
	if e.Resume("boot", 1) == nil {
		t.Fatal("duplicate resume")
	}
}
func TestResumeWithoutLoss(t *testing.T) {
	e, c, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	if err := e.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	c.advance(10*time.Second, 10*time.Second)
	if err := e.Resume("boot", 1); err != nil {
		t.Fatal(err)
	}
	if e.Status().Episode != "initial" {
		t.Fatal(e.Status())
	}
}
func TestSocketStatusAndBounds(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	path := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, e) }()
	var conn net.Conn
	var err error
	for i := 0; i < 100; i++ {
		conn, err = net.Dial("unix", path)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("{\"op\":\"status\"}\n"))
	var response Response
	if err = json.NewDecoder(conn).Decode(&response); err != nil || !response.OK || response.Quality.BootID != "boot" {
		t.Fatal(response, err)
	}
	conn.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
func TestLateExternalBudgetAndFreshness(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	ch.t.Residual = .01
	e.refresh(context.Background())
	if ch.count() != 0 {
		t.Fatal("small correction stepped")
	}
	c.advance(91*time.Second, 91*time.Second)
	ch.t.Residual = 10
	e.refresh(context.Background())
	if ch.count() != 1 || e.Status().Episode != "late-external" {
		t.Fatal(ch.count(), e.Status())
	}
	ch.done <- nil
	waitOutcome(t, e, "completed")
	c.advance(100*time.Second, 100*time.Second)
	e.refresh(context.Background())
	if ch.count() != 1 {
		t.Fatal("late budget renewed")
	}
}
func TestPrepareTimeoutFencesUnknown(t *testing.T) {
	e, _, ch, _, _ := newTestEngine(t)
	e.refresh(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if e.Prepare(ctx) == nil {
		t.Fatal("unresolved step allowed suspend")
	}
	if e.Status().Preparing {
		t.Fatal("preparation not aborted")
	}
	ch.done <- errors.New("late failure")
	waitOutcome(t, e, "unknown")
}
func TestRestartChangesBoot(t *testing.T) {
	e, c, ch, ref, path := newTestEngine(t)
	e.refresh(context.Background())
	newBoot, err := NewEngine(path, "new-boot", c, ch, ref)
	if err != nil || newBoot.Status().BootID != "new-boot" || newBoot.Status().Outcome != "available" {
		t.Fatal(newBoot, err)
	}
	ch.done <- nil
	waitOutcome(t, e, "completed")
}
func TestTimeoutAfterDispatchIsUnknown(t *testing.T) {
	e, _, ch, _, _ := newTestEngine(t)
	e.refresh(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && e.Status().Outcome != "unknown" {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Status().Outcome != "unknown" || ch.count() != 1 {
		t.Fatal(e.Status(), ch.count())
	}
	e.refresh(context.Background())
	if ch.count() != 1 {
		t.Fatal("timeout was retried")
	}
}
func TestHoldoverNeverExternal(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	now, bt, _ := c.Now()
	if err := e.Submit(Request{BootID: "boot", Generation: 1, Source: SourceUser, UTCUnixNS: now.UnixNano(), ReceiptBootNS: int64(bt)}); err != nil {
		t.Fatal(err)
	}
	ch.t = Tracking{Source: SourceUser, External: true, Age: 0, Residual: 0, Uncertainty: 0, Valid: true}
	e.refresh(context.Background())
	if q := e.Status(); q.Source != SourceUser || q.External {
		t.Fatal(q)
	}
	c.advance(31*time.Second, 31*time.Second)
	if e.Status().Source != "unknown" {
		t.Fatal("stale quality")
	}
}
func TestSocketRejectsOversizedAndUnknownFields(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	path := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, e) }()
	defer func() { cancel(); <-done }()
	var c net.Conn
	var err error
	for i := 0; i < 100; i++ {
		c, err = net.Dial("unix", path)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("{\"op\":\"status\",\"bogus\":1}\n"))
	var r Response
	if err = json.NewDecoder(c).Decode(&r); err != nil || r.OK {
		t.Fatal(r, err)
	}
	c.Close()
	c, err = net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte(strings.Repeat("x", MaxRequest+1) + "\n"))
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	if n, _ := c.Read(buf[:]); n != 0 {
		t.Fatal("oversized request answered")
	}
	c.Close()
}
func TestExternalTimeInvalidatesNRF(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	ch.t.Residual = .01
	e.refresh(context.Background())
	now, bt, _ := c.Now()
	r := Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.UnixNano(), ReceiptBootNS: int64(bt)}
	if e.Submit(r) == nil {
		t.Fatal("nrf accepted after external confirmation")
	}
	r.Source = SourceUser
	if err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
}
func TestLocalTrackingCannotStep(t *testing.T) {
	e, _, ch, _, _ := newTestEngine(t)
	ch.t.Source = "local"
	ch.t.External = false
	e.refresh(context.Background())
	if ch.count() != 0 {
		t.Fatal("local reference stepped")
	}
}

type dispatchDelayClock struct {
	base  *fakeClock
	calls int
}

func (c *dispatchDelayClock) Now() (time.Time, time.Duration, error) {
	c.calls++
	if c.calls == 4 {
		c.base.advance(100*time.Second, 100*time.Second)
	}
	return c.base.Now()
}
func TestDispatchRechecksDeadlineAfterPersistence(t *testing.T) {
	base := &fakeClock{utc: time.Unix(1700000000, 0), boot: 10 * time.Second}
	clock := &dispatchDelayClock{base: base}
	ch := &fakeChrony{t: Tracking{Source: "ntp", External: true, Age: time.Second, Residual: 10, Uncertainty: .1, Valid: true}, done: make(chan error, 1)}
	e, err := NewEngine(filepath.Join(t.TempDir(), "state"), "boot", clock, ch, &fakeRef{})
	if err != nil {
		t.Fatal(err)
	}
	e.refresh(context.Background())
	if ch.count() != 0 || e.Status().Outcome != "expired" {
		t.Fatal(ch.count(), e.Status())
	}
}

func TestNRFExternalEstablishmentSurvivesRestartAndResume(t *testing.T) {
	e, c, ch, ref, path := newTestEngine(t)
	ch.t.Residual = .01
	e.refresh(context.Background())
	if e.Status().NRFEligible {
		t.Fatal("external source did not close recovery")
	}
	c.advance(31*time.Second, 31*time.Second)
	restarted, err := NewEngine(path, "boot", c, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Status().NRFEligible {
		t.Fatal("restart reopened external recovery")
	}
	if err = restarted.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.advance(10*time.Second, 10*time.Second)
	if err = restarted.Resume("boot", 1); err != nil {
		t.Fatal(err)
	}
	if restarted.Status().NRFEligible {
		t.Fatal("resume reopened external recovery")
	}
	_, bt, _ := c.Now()
	now, _, _ := c.Now()
	if restarted.Submit(Request{BootID: "boot", Generation: 2, Source: SourceNRF, UTCUnixNS: now.Add(time.Minute).UnixNano(), ReceiptBootNS: int64(bt)}) == nil {
		t.Fatal("nrf admitted after external establishment")
	}
	newBoot, err := NewEngine(path, "new-boot", c, ch, ref)
	if err != nil || !newBoot.Status().NRFEligible {
		t.Fatal("new boot did not reset external latch", err)
	}
}
func TestSubmitDeadlineAndCurrentClock(t *testing.T) {
	e, c, _, ref, _ := newTestEngine(t)
	now, bt, _ := c.Now()
	r := Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.Add(time.Second).UnixNano(), ReceiptBootNS: int64(bt), DeadlineBootNS: int64(bt + 2*time.Second)}
	c.advance(2*time.Second, 2*time.Second)
	if e.Submit(r) == nil || ref.calls != 0 {
		t.Fatal("late sample delivered")
	}
	r.DeadlineBootNS = int64(bt + time.Minute)
	// A separate wall-clock correction must prevent a backwards nRF sample.
	c.advance(time.Minute, 0)
	if e.Submit(r) == nil || ref.calls != 0 {
		t.Fatal("backwards nrf sample delivered")
	}
}

type advancingTracking struct {
	*fakeChrony
	clock *fakeClock
	delay time.Duration
}

func (a *advancingTracking) Tracking(ctx context.Context, now time.Time) (Tracking, error) {
	t, err := a.fakeChrony.Tracking(ctx, now)
	a.clock.advance(a.delay, a.delay)
	return t, err
}
func TestSubmitDeadlineAfterSlowTracking(t *testing.T) {
	e, c, ch, ref, _ := newTestEngine(t)
	now, bt, _ := c.Now()
	e.chrony = &advancingTracking{fakeChrony: ch, clock: c, delay: 3 * time.Second}
	err := e.Submit(Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.Add(time.Minute).UnixNano(), ReceiptBootNS: int64(bt), DeadlineBootNS: int64(bt + 2*time.Second)})
	if err == nil || ref.calls != 0 {
		t.Fatal("slow tracking delivered expired sample")
	}
}
func TestGPSSelectionInvalidatesPendingNRFSample(t *testing.T) {
	e, c, ch, ref, _ := newTestEngine(t)
	now, bt, _ := c.Now()
	if err := e.Submit(Request{BootID: "boot", Generation: 1, Source: SourceGPS, UTCUnixNS: now.UnixNano(), ReceiptBootNS: int64(bt)}); err != nil {
		t.Fatal(err)
	}
	ch.t = Tracking{Source: SourceGPS, Age: time.Second, Residual: .01, Uncertainty: .1, Valid: true}
	if e.Submit(Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.Add(time.Minute).UnixNano(), ReceiptBootNS: int64(bt)}) == nil {
		t.Fatal("GPS transition did not invalidate nrf")
	}
	if ref.calls != 1 || e.Status().NRFEligible {
		t.Fatal("GPS not latched")
	}
}

func TestGPSUncertaintyAndNTPDisagreement(t *testing.T) {
	e, c, ch, ref, _ := newTestEngine(t)
	now, bt, _ := c.Now()
	r := Request{BootID: "boot", Generation: 1, Source: SourceGPS, UTCUnixNS: now.Add(2 * time.Second).UnixNano(), ReceiptBootNS: int64(bt), UncertaintyNS: int64(3 * time.Second)}
	if err := e.Submit(r); err != nil {
		t.Fatalf("bounded GPS uncertainty: %v", err)
	}
	r.UncertaintyNS = int64(3*time.Second) + 1
	if err := e.Submit(r); err == nil {
		t.Fatal("excess uncertainty accepted")
	}
	r.UncertaintyNS = int64(2 * time.Second)
	ch.mu.Lock()
	ch.t = Tracking{Source: "ntp", External: true, Age: time.Second, Residual: .1, Uncertainty: .1, Valid: true}
	ch.mu.Unlock()
	e.refresh(context.Background())
	r.UTCUnixNS = now.Add(20 * time.Second).UnixNano()
	if err := e.Submit(r); err == nil {
		t.Fatal("GPS disagreement with trusted NTP accepted")
	}
	if ref.calls != 1 {
		t.Fatalf("refclock writes: %d", ref.calls)
	}
}

func TestNRFRecoveryAfterExternallySyncedClockLosesSuspendTime(t *testing.T) {
	e, c, ch, ref, path := newTestEngine(t)
	ch.t.Residual = .01
	e.refresh(context.Background())
	if e.Status().NRFEligible {
		t.Fatal("external fence missing")
	}
	if err := e.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.advance(0, 2*time.Hour)
	if err := e.Resume("boot", 1); err != nil {
		t.Fatal(err)
	}
	// A pre-suspend chrony reading that appears recent must not reestablish trust.
	ch.t.Age = time.Second
	e.refresh(context.Background())
	if !e.Status().NRFEligible || e.Status().External {
		t.Fatalf("stale tracking fenced recovery: %+v", e.Status())
	}
	restarted, err := NewEngine(path, "boot", c, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	_, bt, _ := c.Now()
	now, _, _ := c.Now()
	r := Request{BootID: "boot", Generation: 2, Source: SourceNRF, UTCUnixNS: now.Add(2 * time.Hour).UnixNano(), ReceiptBootNS: int64(bt)}
	if err := restarted.Submit(r); err != nil {
		t.Fatalf("nrf recovery: %v", err)
	}
	ch.mu.Lock()
	ch.t = Tracking{Source: SourceNRF, Age: 0, Residual: .1, Uncertainty: .1, Valid: true}
	ch.mu.Unlock()
	restarted.refresh(context.Background())
	if q := restarted.Status(); q.Source != SourceNRF || q.External {
		t.Fatalf("wrong holdover: %+v", q)
	}
}
func TestResumeExternalRequiresPostResumeObservation(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	ch.t.Residual = .01
	e.refresh(context.Background())
	if err := e.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.advance(0, time.Hour)
	if err := e.Resume("boot", 1); err != nil {
		t.Fatal(err)
	}
	c.advance(7*time.Second, 7*time.Second)
	ch.mu.Lock()
	ch.t.Age = 8 * time.Second
	ch.mu.Unlock()
	e.refresh(context.Background())
	if !e.Status().NRFEligible {
		t.Fatal("pre-sleep observation accepted")
	}
	ch.mu.Lock()
	ch.t.Age = time.Second
	ch.mu.Unlock()
	e.refresh(context.Background())
	if e.Status().NRFEligible || !e.Status().External {
		t.Fatal("new external observation not accepted")
	}
}

func TestGPSOnlyOfflineConvergesFromSelectedQuality(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	now, bt, _ := c.Now()
	r := Request{BootID: "boot", Generation: 1, Source: SourceGPS, UTCUnixNS: now.Add(time.Second).UnixNano(), ReceiptBootNS: int64(bt), UncertaintyNS: int64(2 * time.Second)}
	if err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	ch.mu.Lock()
	ch.t = Tracking{Source: SourceGPS, External: true, Age: 0, Residual: .1, Uncertainty: .1, Valid: true}
	ch.mu.Unlock()
	e.refresh(context.Background())
	q := e.Status()
	if q.Source != "gps" || !q.External || q.UncertaintySeconds < 2 {
		t.Fatalf("GPS quality: %+v", q)
	}
	ch.mu.Lock()
	ch.t.Uncertainty = 4.1
	ch.mu.Unlock()
	e.refresh(context.Background())
	if q = e.Status(); q.UncertaintySeconds != 4.1 || !q.External {
		t.Fatalf("configured precision was counted twice: %+v", q)
	}
}

func TestNetworkHoldoverLateThenExternalRecovery(t *testing.T) {
	e, c, ch, ref, path := newTestEngine(t)
	c.advance(2*time.Hour, 2*time.Hour)
	ch.t = Tracking{Source: SourceNetworkHoldover, Age: time.Second, Residual: 6 * 3600, Uncertainty: .1, Valid: true}
	e.refresh(context.Background())
	if ch.count() != 1 || e.Status().Episode != "late-holdover" {
		t.Fatal(e.Status(), ch.count())
	}
	ch.done <- nil
	waitOutcome(t, e, "completed")
	e.refresh(context.Background())
	if e.Status().External || e.Status().Source != SourceNetworkHoldover {
		t.Fatal(e.Status())
	}
	c.advance(episodeWindow+time.Second, episodeWindow+time.Second)
	e.refresh(context.Background())
	if ch.count() != 1 {
		t.Fatal("repeated holdover recovery")
	}
	restarted, err := NewEngine(path, "boot", c, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	restarted.refresh(context.Background())
	if ch.count() != 1 {
		t.Fatal("restart repeated holdover recovery")
	}
	ch.t = Tracking{Source: "ntp", External: true, Age: time.Second, Residual: -4 * 3600, Uncertainty: .1, Valid: true}
	restarted.refresh(context.Background())
	if ch.count() != 2 || restarted.Status().Episode != "late-external" {
		t.Fatal(restarted.Status(), ch.count())
	}
	ch.done <- nil
	waitOutcome(t, restarted, "completed")
}

func TestSingleHoldoverSampleUsesSkewBoundForStep(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	now, bt, _ := c.Now()
	r := Request{BootID: "boot", Generation: 1, Source: SourceUser, UTCUnixNS: now.Add(20 * time.Second).UnixNano(), ReceiptBootNS: int64(bt), UncertaintyNS: int64(time.Second)}
	if err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Second, time.Second)
	ch.t = Tracking{Source: SourceUser, Age: time.Second, Residual: 20, Uncertainty: 6, SkewPPM: 1e6, Valid: true}
	e.refresh(context.Background())
	if ch.count() != 1 {
		t.Fatalf("single-sample recovery not dispatched: %+v", e.Status())
	}
	ch.done <- nil
	waitOutcome(t, e, "completed")
}

func TestControlSocketTransportAndSuspendUnknown(t *testing.T) {
	e, clock, chrony, _, statePath := newTestEngine(t)
	dir := t.TempDir()
	datagram, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(dir, "user.sock"), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer datagram.Close()
	e.ref = SockRefclock{Dir: dir}
	path := filepath.Join(dir, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, e) }()
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket unavailable")
		}
	}
	call := func(req Request) Response {
		t.Helper()
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err = json.NewEncoder(conn).Encode(req); err != nil {
			t.Fatal(err)
		}
		var response Response
		if err = json.NewDecoder(conn).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	status := call(Request{Op: "status"})
	if !status.OK {
		t.Fatal(status)
	}
	now, bt, _ := clock.Now()
	result := call(Request{Op: "submit", BootID: status.Quality.BootID, Generation: status.Quality.Generation, Source: SourceUser, UTCUnixNS: now.Add(20 * time.Second).UnixNano(), ReceiptBootNS: int64(bt), UncertaintyNS: int64(time.Second)})
	if !result.OK {
		t.Fatal(result)
	}
	datagram.SetReadDeadline(time.Now().Add(time.Second))
	packet := make([]byte, 64)
	n, _, err := datagram.ReadFromUnix(packet)
	if err != nil || n < 32 {
		t.Fatal(n, err)
	}
	chrony.t = Tracking{Source: SourceUser, Age: time.Second, Residual: 20, Uncertainty: 1, Valid: true}
	e.refresh(context.Background())
	if chrony.count() != 1 {
		t.Fatal("missing step")
	}
	chrony.done <- errors.New("response lost")
	waitOutcome(t, e, "unknown")
	if r := call(Request{Op: "prepare"}); r.OK || r.Quality.Preparing {
		t.Fatal("unknown outcome allowed suspend", r)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(statePath, "boot", clock, chrony, e.ref)
	if err != nil {
		t.Fatal(err)
	}
	restarted.refresh(context.Background())
	if chrony.count() != 1 || restarted.Status().Outcome != "unknown" {
		t.Fatal("unknown step replayed", restarted.Status())
	}
}

func TestPrepareBaselineAfterInFlightClockStep(t *testing.T) {
	for _, step := range []time.Duration{-6 * time.Hour, 6 * time.Hour} {
		for _, lost := range []bool{false, true} {
			t.Run(fmt.Sprintf("step=%s/lost=%t", step, lost), func(t *testing.T) {
				e, c, ch, _, _ := newTestEngine(t)
				e.refresh(context.Background())
				prepared := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					prepared <- e.Prepare(ctx)
				}()
				deadline := time.Now().Add(time.Second)
				for !e.Status().Preparing {
					if time.Now().After(deadline) {
						t.Fatal("no preparation fence")
					}
					time.Sleep(time.Millisecond)
				}
				c.advance(step, 0)
				ch.done <- nil
				if err := <-prepared; err != nil {
					t.Fatal(err)
				}
				if lost {
					c.advance(time.Second, 11*time.Second)
				} else {
					c.advance(time.Second, time.Second)
				}
				if err := e.Resume("boot", 1); err != nil {
					t.Fatal(err)
				}
				if got := e.Status().Episode; (got == "resume") != lost {
					t.Fatalf("episode=%s", got)
				}
			})
		}
	}
}

func TestRestartDuringPrepareWaitDoesNotCreateResumeEpisode(t *testing.T) {
	e, c, ch, ref, path := newTestEngine(t)
	e.state.Preparing = true
	e.state.PrepareBaselineValid = false
	e.state.Outcome = "in-flight"
	e.state.DispatchBoot = int64(10 * time.Second)
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(path, "boot", c, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(-6*time.Hour, 10*time.Second)
	if err := restarted.Resume("boot", 1); err != nil {
		t.Fatal(err)
	}
	q := restarted.Status()
	if q.Preparing || q.Episode != "initial" || q.Outcome != "unknown" {
		t.Fatal(q)
	}
}

func TestNRFSampleSurvivesCoordinatorRestartBeforeDispatch(t *testing.T) {
	e, c, ch, ref, path := newTestEngine(t)
	now, boot, _ := c.Now()
	r := Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.Add(6 * time.Hour).UnixNano(), ReceiptBootNS: int64(boot), UncertaintyNS: int64(time.Second)}
	if err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(path, "boot", c, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	ch.t = Tracking{Source: SourceNRF, Age: 0, Residual: (6 * time.Hour).Seconds(), Uncertainty: 6, SkewPPM: 1e6, Valid: true}
	restarted.refresh(context.Background())
	if ch.count() != 1 || restarted.Status().Outcome != "in-flight" {
		t.Fatal(restarted.Status(), ch.count())
	}
	ch.done <- nil
	waitOutcome(t, restarted, "completed")
}

func TestUnknownNRFStepReconcilesAfterRestart(t *testing.T) {
	e, c, ch, ref, path := newTestEngine(t)
	now, boot, _ := c.Now()
	r := Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.Add(6 * time.Hour).UnixNano(), ReceiptBootNS: int64(boot), UncertaintyNS: int64(time.Second)}
	if err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	ch.t = Tracking{Source: SourceNRF, Age: 0, Residual: (6 * time.Hour).Seconds(), Uncertainty: 6, SkewPPM: 1e6, Valid: true}
	e.refresh(context.Background())
	c.advance(6*time.Hour, 0)
	ch.done <- errors.New("lost reply")
	waitOutcome(t, e, "unknown")
	e.mu.Lock()
	e.state.Preparing = true
	e.state.PrepareBaselineValid = false
	if err := e.save(); err != nil {
		e.mu.Unlock()
		t.Fatal(err)
	}
	e.mu.Unlock()
	restarted, err := NewEngine(path, "boot", c, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Resume("boot", 1); err != nil {
		t.Fatal(err)
	}
	c.advance(11*time.Second, 11*time.Second)
	ch.t = Tracking{Valid: false}
	restarted.refresh(context.Background())
	if q := restarted.Status(); q.Outcome != "reconciled" || ch.count() != 1 {
		t.Fatal(q, ch.count())
	}
	if err := restarted.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownNRFStepDoesNotReconcileWithoutObservedMovement(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	now, boot, _ := c.Now()
	r := Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.Add(6 * time.Hour).UnixNano(), ReceiptBootNS: int64(boot), UncertaintyNS: int64(time.Second)}
	if err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	ch.t = Tracking{Source: SourceNRF, Residual: (6 * time.Hour).Seconds(), Uncertainty: 6, SkewPPM: 1e6, Valid: true}
	e.refresh(context.Background())
	ch.done <- errors.New("lost reply")
	waitOutcome(t, e, "unknown")
	c.advance(11*time.Second, 11*time.Second)
	ch.t.Residual = .01
	e.refresh(context.Background())
	if q := e.Status(); q.Outcome != "unknown" || ch.count() != 1 {
		t.Fatal(q, ch.count())
	}
	if err := e.Prepare(context.Background()); err == nil {
		t.Fatal("unresolved outcome permitted suspend")
	}
}

func TestPersistedNRFSampleExpiresAndDoesNotCrossBoot(t *testing.T) {
	e, c, ch, ref, path := newTestEngine(t)
	now, boot, _ := c.Now()
	if err := e.Submit(Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.Add(time.Hour).UnixNano(), ReceiptBootNS: int64(boot), UncertaintyNS: int64(time.Second)}); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(path, "boot", c, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(31*time.Second, 31*time.Second)
	ch.t = Tracking{Source: SourceNRF, Residual: 3600, Valid: true}
	restarted.refresh(context.Background())
	if ch.count() != 0 {
		t.Fatal("expired sample dispatched")
	}
	rebooted, err := NewEngine(path, "new-boot", c, ch, ref)
	if err != nil {
		t.Fatal(err)
	}
	rebooted.refresh(context.Background())
	if ch.count() != 0 {
		t.Fatal("cross-boot sample dispatched")
	}
}

func TestUnknownHoldoverDoesNotReconcileAgainstReplacementSample(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	now, boot, _ := c.Now()
	first := Request{BootID: "boot", Generation: 1, Source: SourceUser, UTCUnixNS: now.Add(time.Hour).UnixNano(), ReceiptBootNS: int64(boot), UncertaintyNS: int64(time.Second)}
	if err := e.Submit(first); err != nil {
		t.Fatal(err)
	}
	ch.t = Tracking{Source: SourceUser, Residual: 3600, Uncertainty: 1, Valid: true}
	e.refresh(context.Background())
	c.advance(time.Hour, 0)
	ch.done <- errors.New("lost reply")
	waitOutcome(t, e, "unknown")
	c.advance(time.Second, time.Second)
	now, boot, _ = c.Now()
	replacement := Request{BootID: "boot", Generation: 1, Source: SourceUser, UTCUnixNS: now.Add(time.Second).UnixNano(), ReceiptBootNS: int64(boot), UncertaintyNS: int64(time.Second)}
	if err := e.Submit(replacement); err != nil {
		t.Fatal(err)
	}
	c.advance(10*time.Second, 10*time.Second)
	ch.t = Tracking{Valid: false}
	e.refresh(context.Background())
	if e.Status().Outcome != "unknown" {
		t.Fatal("replacement evidence reconciled old dispatch", e.Status())
	}
}

func TestRetainedNRFDoesNotDispatchAfterExternalEstablishment(t *testing.T) {
	for _, source := range []string{"ntp", SourceGPS} {
		for _, restart := range []bool{false, true} {
			for _, residual := range []float64{-20, 20} {
				t.Run(fmt.Sprintf("%s/restart=%t/residual=%g", source, restart, residual), func(t *testing.T) {
					e, c, ch, ref, path := newTestEngine(t)
					now, bt, _ := c.Now()
					ch.t.Valid = false
					if err := e.Submit(Request{BootID: "boot", Generation: 1, Source: SourceNRF, UTCUnixNS: now.Add(20 * time.Second).UnixNano(), ReceiptBootNS: int64(bt)}); err != nil {
						t.Fatal(err)
					}
					if source == SourceGPS {
						if err := e.Submit(Request{BootID: "boot", Generation: 1, Source: SourceGPS, UTCUnixNS: now.UnixNano(), ReceiptBootNS: int64(bt)}); err != nil {
							t.Fatal(err)
						}
					}
					ch.t = Tracking{Source: source, External: true, Residual: .1, Uncertainty: .1, Valid: true}
					e.refresh(context.Background())
					if !e.state.ExternalEstablished {
						t.Fatal("external time not established")
					}
					if restart {
						var err error
						e, err = NewEngine(path, "boot", c, ch, ref)
						if err != nil {
							t.Fatal(err)
						}
					}
					if _, ok := e.samples[SourceNRF]; !ok {
						t.Fatal("retained NRF evidence missing")
					}
					ch.t = Tracking{Source: SourceNRF, Residual: residual, Uncertainty: .1, Valid: true}
					e.refresh(context.Background())
					if ch.count() != 0 || e.Status().Outcome != "available" {
						t.Fatal("retained NRF authorized dispatch", ch.count(), e.Status())
					}
				})
			}
		}
	}
}

func TestGPSSubmitFutureClockSOCKTime64(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	c.utc = time.Date(2040, 1, 1, 0, 0, 0, 123456000, time.UTC)
	ch.t.Valid = false
	dir := t.TempDir()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(dir, "gps.sock"), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	e.ref = SockRefclock{Dir: dir}
	now, bt, _ := c.Now()
	gps := time.Date(2026, 1, 1, 0, 0, 0, 123456000, time.UTC)
	if err := e.Submit(Request{BootID: "boot", Generation: 1, Source: SourceGPS, UTCUnixNS: gps.UnixNano(), ReceiptBootNS: int64(bt)}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 40 || int64(binary.NativeEndian.Uint64(buf[:8])) != now.Unix() || binary.NativeEndian.Uint64(buf[8:16]) != 123456 || math.Float64frombits(binary.NativeEndian.Uint64(buf[16:24])) != gps.Sub(now).Seconds() || binary.NativeEndian.Uint32(buf[36:40]) != 0x534f434b {
		t.Fatalf("invalid time64 packet: %x", buf[:n])
	}
}

func TestNRFDispatchAfterLostTimeAndExternalChangeDuringDispatch(t *testing.T) {
	e, c, ch, _, _ := newTestEngine(t)
	ch.t.Residual = .01
	e.refresh(context.Background())
	if !e.state.ExternalEstablished {
		t.Fatal("external time not established")
	}
	if err := e.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.advance(0, time.Hour)
	if err := e.Resume("boot", 1); err != nil {
		t.Fatal(err)
	}
	ch.t.Valid = false
	now, bt, _ := c.Now()
	if err := e.Submit(Request{BootID: "boot", Generation: 2, Source: SourceNRF, UTCUnixNS: now.Add(time.Hour).UnixNano(), ReceiptBootNS: int64(bt)}); err != nil {
		t.Fatal(err)
	}
	ch.t = Tracking{Source: SourceNRF, Residual: 3600, Uncertainty: 1, Valid: true}
	e.refresh(context.Background())
	if ch.count() != 1 || e.Status().Outcome != "in-flight" {
		t.Fatal("resume dispatch missing", e.Status())
	}
	// The source can change after admission; chrony applies its current correction.
	c.advance(7*time.Second, 7*time.Second)
	ch.t = Tracking{Source: "ntp", External: true, Age: time.Second, Residual: .01, Uncertainty: .1, Valid: true}
	e.refresh(context.Background())
	if !e.state.ExternalEstablished || ch.count() != 1 {
		t.Fatal("external observation during dispatch", e.Status())
	}
	ch.done <- nil
	waitOutcome(t, e, "completed")
}
