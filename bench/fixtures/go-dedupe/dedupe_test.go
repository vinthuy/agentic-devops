package dedupe

import (
	"reflect"
	"testing"
)

func TestDedupeKeepsFirstOccurrenceOrder(t *testing.T) {
	got := Dedupe([]string{"b", "a", "b", "c", "a"})
	want := []string{"b", "a", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Dedupe() = %v，期望 %v", got, want)
	}
}

func TestDedupeEmpty(t *testing.T) {
	if got := Dedupe(nil); len(got) != 0 {
		t.Fatalf("Dedupe(nil) 应为空，实际 %v", got)
	}
}
