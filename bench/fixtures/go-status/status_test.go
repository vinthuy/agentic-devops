package status

import "testing"

func TestTextKnownCodes(t *testing.T) {
	cases := map[int]string{
		200: "OK",
		201: "Created",
		204: "No Content",
		400: "Bad Request",
		404: "Not Found",
		500: "Internal Server Error",
	}
	for code, want := range cases {
		if got := Text(code); got != want {
			t.Errorf("Text(%d) = %q，期望 %q", code, got, want)
		}
	}
}

func TestTextUnknown(t *testing.T) {
	if got := Text(599); got != "Unknown" {
		t.Errorf("Text(599) = %q", got)
	}
}
