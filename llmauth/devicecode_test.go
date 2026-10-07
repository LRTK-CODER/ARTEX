package llmauth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestStartDeviceCode(t *testing.T) {
	f := newFakeAuthServer(t, time.Unix(1_800_000_000, 0))
	got, err := f.client().StartDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceCode: %v", err)
	}
	want := DeviceCode{DeviceAuthID: "dev-1", UserCode: "ABCD-1234", VerificationURL: f.srv.URL + "/codex/device", Interval: 5 * time.Second}
	if got != want {
		t.Fatalf("StartDeviceCode = %+v, want %+v", got, want)
	}
}

func TestStartDeviceCodeErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantErr  error
		wantCode int
	}{
		{name: "꺼진 계정", status: http.StatusNotFound, wantErr: ErrDeviceCodeDisabled},
		{name: "서버 오류", status: http.StatusInternalServerError, wantCode: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAuthServer(t, time.Unix(1_800_000_000, 0))
			f.usercodeCode = tc.status
			_, err := f.client().StartDeviceCode(context.Background())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			var tokenErr *TokenError
			if !errors.As(err, &tokenErr) || tokenErr.StatusCode != tc.wantCode {
				t.Fatalf("err = %v, want TokenError status %d", err, tc.wantCode)
			}
		})
	}
}

func TestWaitDeviceCode(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	f.pollStatuses = []int{http.StatusForbidden, http.StatusNotFound, http.StatusForbidden}
	clock := &fakeClock{now: now}
	c := f.client()
	c.now, c.sleep = clock.Now, clock.Sleep
	dc := DeviceCode{DeviceAuthID: "dev-1", UserCode: "ABCD-1234", Interval: 5 * time.Second}

	got, err := c.WaitDeviceCode(context.Background(), dc)
	if err != nil {
		t.Fatalf("WaitDeviceCode: %v", err)
	}
	if got.AccountID != "acct-123" || got.RefreshToken == "" {
		t.Fatalf("WaitDeviceCode = %+v", got)
	}
	if len(clock.sleeps) != 3 || clock.sleeps[0] != 5*time.Second {
		t.Fatalf("sleeps = %v, want three 5s waits", clock.sleeps)
	}
	form := f.exchangeForms[0]
	if form.Get("code") != "device-auth-code" || form.Get("code_verifier") != "device-verifier" ||
		form.Get("redirect_uri") != f.srv.URL+"/deviceauth/callback" {
		t.Fatalf("exchange form = %v", form)
	}
}

func TestWaitDeviceCodeZeroIntervalUsesDefault(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	f.pollStatuses = []int{http.StatusForbidden}
	clock := &fakeClock{now: now}
	c := f.client()
	c.now, c.sleep = clock.Now, clock.Sleep

	if _, err := c.WaitDeviceCode(context.Background(), DeviceCode{DeviceAuthID: "dev-1", UserCode: "ABCD-1234"}); err != nil {
		t.Fatalf("WaitDeviceCode: %v", err)
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != 5*time.Second {
		t.Fatalf("sleeps = %v, want one 5s wait", clock.sleeps)
	}
}

func TestWaitDeviceCodeTimesOut(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	f.isAlwaysPending = true
	clock := &fakeClock{now: now}
	c := f.client()
	c.now, c.sleep = clock.Now, clock.Sleep
	dc := DeviceCode{DeviceAuthID: "dev-1", UserCode: "ABCD-1234", Interval: 7 * time.Minute}

	_, err := c.WaitDeviceCode(context.Background(), dc)
	if !errors.Is(err, ErrDeviceCodeTimeout) {
		t.Fatalf("err = %v, want ErrDeviceCodeTimeout", err)
	}
	// 7분, 7분 다음 남은 1분만 기다리고 15분에서 멈춘다.
	want := []time.Duration{7 * time.Minute, 7 * time.Minute, time.Minute}
	if len(clock.sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", clock.sleeps, want)
	}
	for i := range want {
		if clock.sleeps[i] != want[i] {
			t.Fatalf("sleeps = %v, want %v", clock.sleeps, want)
		}
	}
}

func TestWaitDeviceCodeStopsOnContextCancel(t *testing.T) {
	f := newFakeAuthServer(t, time.Unix(1_800_000_000, 0))
	f.isAlwaysPending = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.client().WaitDeviceCode(ctx, DeviceCode{DeviceAuthID: "dev-1", UserCode: "ABCD-1234", Interval: time.Hour})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
