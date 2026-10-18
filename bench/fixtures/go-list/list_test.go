package list

import (
	"reflect"
	"testing"
)

func TestParseTrimsAndDropsEmpty(t *testing.T) {
	got := Parse("a, b,, c ")
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse() = %#v，期望 %#v", got, want)
	}
}

func TestParseSingle(t *testing.T) {
	if got := Parse("solo"); !reflect.DeepEqual(got, []string{"solo"}) {
		t.Fatalf("Parse(\"solo\") = %#v", got)
	}
}

func TestParseEmpty(t *testing.T) {
	if got := Parse(""); len(got) != 0 {
		t.Fatalf("Parse(\"\") 应为空，实际 %#v", got)
	}
}
