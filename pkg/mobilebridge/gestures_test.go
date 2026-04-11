package mobilebridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// proxyWithSender builds a minimal Proxy wired to a fake messageSender so
// gesture tests can drive the method receivers without a real WebSocket.
func proxyWithSender(s messageSender) *Proxy {
	return &Proxy{senderOverride: s}
}

// fakeSender captures every message a gesture helper produces.
type fakeSender struct {
	msgs []TouchEventParams
	err  error
}

func (f *fakeSender) sendUpstream(method string, params any) error {
	if f.err != nil {
		return f.err
	}
	if method != "Input.dispatchTouchEvent" {
		return nil
	}
	// Round-trip through JSON so tests exercise the real marshal path.
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var p TouchEventParams
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	f.msgs = append(f.msgs, p)
	return nil
}

func TestTapProducesStartEnd(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	if err := p.Tap(context.Background(), 100, 200); err != nil {
		t.Fatal(err)
	}
	if len(f.msgs) != 2 {
		t.Fatalf("want 2 events, got %d", len(f.msgs))
	}
	if f.msgs[0].Type != "touchStart" {
		t.Errorf("first event type = %q", f.msgs[0].Type)
	}
	if len(f.msgs[0].TouchPoints) != 1 {
		t.Fatalf("want 1 touch point, got %d", len(f.msgs[0].TouchPoints))
	}
	pt := f.msgs[0].TouchPoints[0]
	if pt.X != 100 || pt.Y != 200 {
		t.Errorf("coords = (%v, %v)", pt.X, pt.Y)
	}
	if f.msgs[1].Type != "touchEnd" {
		t.Errorf("second event type = %q", f.msgs[1].Type)
	}
	if len(f.msgs[1].TouchPoints) != 0 {
		t.Errorf("touchEnd should carry zero points, got %d", len(f.msgs[1].TouchPoints))
	}
}

func TestLongPress(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	if err := p.LongPress(context.Background(), 10, 20, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.msgs) != 2 || f.msgs[0].Type != "touchStart" || f.msgs[1].Type != "touchEnd" {
		t.Errorf("unexpected sequence: %#v", f.msgs)
	}
}

func TestSwipeSequence(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	if err := p.Swipe(context.Background(), 0, 0, 100, 200, 0); err != nil {
		t.Fatal(err)
	}
	// Expect: touchStart + 10 moves + final move + touchEnd = 13 events.
	if len(f.msgs) != 13 {
		t.Fatalf("want 13 events, got %d", len(f.msgs))
	}
	if f.msgs[0].Type != "touchStart" {
		t.Errorf("first = %q", f.msgs[0].Type)
	}
	if f.msgs[len(f.msgs)-1].Type != "touchEnd" {
		t.Errorf("last = %q", f.msgs[len(f.msgs)-1].Type)
	}
	// Start point matches from*
	if f.msgs[0].TouchPoints[0].X != 0 || f.msgs[0].TouchPoints[0].Y != 0 {
		t.Errorf("start coords wrong: %#v", f.msgs[0].TouchPoints[0])
	}
	// Final move point matches to*
	final := f.msgs[len(f.msgs)-2]
	if final.Type != "touchMove" || final.TouchPoints[0].X != 100 || final.TouchPoints[0].Y != 200 {
		t.Errorf("final move wrong: %#v", final)
	}
	// Intermediate moves should be monotonically progressing.
	prev := 0.0
	for i := 1; i < 12; i++ {
		if f.msgs[i].Type != "touchMove" {
			t.Errorf("msgs[%d] type = %q", i, f.msgs[i].Type)
		}
		x := f.msgs[i].TouchPoints[0].X
		if x < prev {
			t.Errorf("non-monotonic x at i=%d: %v < %v", i, x, prev)
		}
		prev = x
	}
}

func TestPinchTwoFingers(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	if err := p.Pinch(context.Background(), 500, 500, 0.5); err != nil {
		t.Fatal(err)
	}
	// touchStart + 10 moves + touchEnd = 12
	if len(f.msgs) != 12 {
		t.Fatalf("want 12 events, got %d", len(f.msgs))
	}
	start := f.msgs[0]
	if start.Type != "touchStart" {
		t.Fatal("first not touchStart")
	}
	if len(start.TouchPoints) != 2 {
		t.Fatalf("pinch start should have 2 points, got %d", len(start.TouchPoints))
	}
	// IDs must differ so Chrome treats them as separate fingers.
	if start.TouchPoints[0].ID == start.TouchPoints[1].ID {
		t.Errorf("pinch fingers share ID: %#v", start.TouchPoints)
	}
	// Symmetric around center.
	a, b := start.TouchPoints[0], start.TouchPoints[1]
	if round((a.X+b.X)/2) != 500 {
		t.Errorf("pinch not centered: %v, %v", a.X, b.X)
	}
	// Pinch-in: fingers end closer together than they started.
	finalMove := f.msgs[10]
	if finalMove.Type != "touchMove" {
		t.Fatalf("expected touchMove, got %q", finalMove.Type)
	}
	startSpread := b.X - a.X
	endSpread := finalMove.TouchPoints[1].X - finalMove.TouchPoints[0].X
	if endSpread >= startSpread {
		t.Errorf("pinch-in should shrink spread: start=%v end=%v", startSpread, endSpread)
	}
}

func TestPinchOutExpands(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	if err := p.Pinch(context.Background(), 500, 500, 2.0); err != nil {
		t.Fatal(err)
	}
	start := f.msgs[0]
	finalMove := f.msgs[10]
	startSpread := start.TouchPoints[1].X - start.TouchPoints[0].X
	endSpread := finalMove.TouchPoints[1].X - finalMove.TouchPoints[0].X
	if endSpread <= startSpread {
		t.Errorf("pinch-out should grow spread: start=%v end=%v", startSpread, endSpread)
	}
}

func TestSwipe_BoundsValidation(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	cases := [][5]int{
		{-1, 0, 10, 10, 0},
		{0, -1, 10, 10, 0},
		{0, 0, -10, 10, 0},
		{0, 0, 10, -10, 0},
		{maxCoord + 1, 0, 10, 10, 0},
		{0, 0, maxCoord + 1, 10, 0},
	}
	for _, c := range cases {
		if err := p.Swipe(context.Background(), c[0], c[1], c[2], c[3], c[4]); err == nil {
			t.Errorf("expected error for %v", c)
		}
	}
	if len(f.msgs) != 0 {
		t.Errorf("no events should have been emitted on error, got %d", len(f.msgs))
	}
}

func TestPinch_ScaleZero(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	if err := p.Pinch(context.Background(), 500, 500, 0); err == nil {
		t.Error("expected error for scale=0")
	}
	if len(f.msgs) != 0 {
		t.Error("no events should be emitted on scale=0")
	}
}

func TestLongPress_NegativeDuration(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	if err := p.LongPress(context.Background(), 100, 100, -1); err == nil {
		t.Error("expected error for negative duration")
	}
	if err := p.LongPress(context.Background(), 100, 100, 0); err == nil {
		t.Error("expected error for zero duration")
	}
	if len(f.msgs) != 0 {
		t.Error("no events should be emitted on bad duration")
	}
}

func TestTap_BasicEventSequence(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	if err := p.Tap(context.Background(), 42, 84); err != nil {
		t.Fatal(err)
	}
	if len(f.msgs) != 2 {
		t.Fatalf("want 2 events, got %d", len(f.msgs))
	}
	if f.msgs[0].Type != "touchStart" {
		t.Errorf("first event should be touchStart, got %q", f.msgs[0].Type)
	}
	if f.msgs[1].Type != "touchEnd" {
		t.Errorf("second event should be touchEnd, got %q", f.msgs[1].Type)
	}
	if len(f.msgs[0].TouchPoints) != 1 {
		t.Fatalf("touchStart should have 1 point, got %d", len(f.msgs[0].TouchPoints))
	}
	pt := f.msgs[0].TouchPoints[0]
	if pt.X != 42 || pt.Y != 84 {
		t.Errorf("touchStart coords = (%v,%v)", pt.X, pt.Y)
	}
	if len(f.msgs[1].TouchPoints) != 0 {
		t.Errorf("touchEnd should carry 0 points, got %d", len(f.msgs[1].TouchPoints))
	}
}

func TestPinchRejectsBadScale(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	if err := p.Pinch(context.Background(), 0, 0, 0); err == nil {
		t.Error("expected error for scale=0")
	}
	if err := p.Pinch(context.Background(), 0, 0, -1); err == nil {
		t.Error("expected error for negative scale")
	}
}

func TestBuildTouchEventJSONShape(t *testing.T) {
	ev := buildTouchEvent("touchStart", []TouchPoint{{X: 1, Y: 2, ID: 0, Force: 1}})
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	// Minimum required CDP keys must be present.
	want := `"type":"touchStart"`
	if !contains(string(b), want) {
		t.Errorf("missing %s in %s", want, b)
	}
	if !contains(string(b), `"touchPoints":[`) {
		t.Errorf("missing touchPoints array in %s", b)
	}
	if !contains(string(b), `"x":1`) || !contains(string(b), `"y":2`) {
		t.Errorf("missing coords in %s", b)
	}
}

// TestLongPressRespectsContextCancellation verifies LongPress bails out of
// its sleep when ctx is canceled, instead of blocking for the full duration.
func TestLongPressRespectsContextCancellation(t *testing.T) {
	f := &fakeSender{}
	p := proxyWithSender(f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- p.LongPress(ctx, 10, 20, 60000) // 60s; would hang pre-M4
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected ctx.Err, got nil")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("LongPress did not honor ctx cancellation")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
