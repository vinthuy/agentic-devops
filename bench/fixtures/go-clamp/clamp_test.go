package clamp

import "testing"

func TestClampWithinRange(t *testing.T) {
	if got := Clamp(5, 0, 10); got != 5 {
		t.Fatalf("Clamp(5,0,10) = %d", got)
	}
}

func TestClampBelowLow(t *testing.T) {
	if got := Clamp(-3, 0, 10); got != 0 {
		t.Fatalf("Clamp(-3,0,10) = %d，期望 0", got)
	}
}

func TestClampAboveHigh(t *testing.T) {
	if got := Clamp(99, 0, 10); got != 10 {
		t.Fatalf("Clamp(99,0,10) = %d，期望 10", got)
	}
}
