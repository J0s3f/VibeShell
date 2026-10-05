package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdmissionRejectsWhenQueueFull(t *testing.T) {
	admission, err := NewAdmission(AdmissionConfig{MaxConcurrent: 1, MaxWaiting: 0})
	if err != nil {
		t.Fatalf("NewAdmission: %v", err)
	}
	release, err := admission.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	if _, err := admission.acquire(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire error = %v, want ErrBusy", err)
	}
}

func TestAdmissionHonorsContextWhileWaiting(t *testing.T) {
	admission, err := NewAdmission(AdmissionConfig{MaxConcurrent: 1, MaxWaiting: 1})
	if err != nil {
		t.Fatalf("NewAdmission: %v", err)
	}
	release, err := admission.acquire(context.Background())
	if err != nil {
		t.Fatalf("hold slot: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := admission.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire error = %v, want context.Canceled", err)
	}
}

func TestAdmissionRejectsInvalidConfig(t *testing.T) {
	if _, err := NewAdmission(AdmissionConfig{MaxConcurrent: 0, MaxWaiting: 1}); err == nil {
		t.Error("MaxConcurrent 0 accepted")
	}
	if _, err := NewAdmission(AdmissionConfig{MaxConcurrent: 1, MaxWaiting: -1}); err == nil {
		t.Error("negative MaxWaiting accepted")
	}
}

func TestAdmissionBoundsConcurrentWork(t *testing.T) {
	const (
		maxConcurrent = 3
		waiting       = 64
		workers       = 64
	)
	admission, err := NewAdmission(AdmissionConfig{MaxConcurrent: maxConcurrent, MaxWaiting: waiting})
	if err != nil {
		t.Fatalf("NewAdmission: %v", err)
	}

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := admission.acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			current := inFlight.Add(1)
			for {
				observed := peak.Load()
				if current <= observed || peak.CompareAndSwap(observed, current) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-1)
			release()
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > maxConcurrent {
		t.Errorf("peak concurrent work = %d, want <= %d", got, maxConcurrent)
	}
	if got := inFlight.Load(); got != 0 {
		t.Errorf("in-flight after release = %d, want 0", got)
	}
	if got := admission.inWork.Load(); got != 0 {
		t.Errorf("admission.inWork after release = %d, want 0", got)
	}
}
