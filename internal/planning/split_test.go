package planning

import "testing"

func TestValidateSplitRequiresOrderedSlices(t *testing.T) {
	names := map[string]bool{"check": true}
	one := SplitPlan{Slices: []Slice{{
		Summary: "只有一段", AllowedPaths: []string{"a.txt"},
		Acceptance: []Acceptance{{
			ID: "a1", Description: "a.txt 存在",
			Check: Check{Type: CheckFileExists, Path: "a.txt"},
		}},
	}}}
	if err := ValidateSplit(one, names); err == nil {
		t.Fatal("只有一段不能当作拆分通过")
	}
	ok := SplitPlan{Slices: []Slice{
		{
			Summary: "第一段", AllowedPaths: []string{"a.txt"},
			Acceptance: []Acceptance{{
				ID: "a1", Description: "a.txt 存在",
				Check: Check{Type: CheckFileExists, Path: "a.txt"},
			}},
		},
		{
			Summary: "第二段", AllowedPaths: []string{"b.txt"},
			Acceptance: []Acceptance{{
				ID: "b1", Description: "b.txt 存在",
				Check: Check{Type: CheckFileExists, Path: "b.txt"},
			}},
		},
	}}
	if err := ValidateSplit(ok, names); err != nil {
		t.Fatal(err)
	}
}
