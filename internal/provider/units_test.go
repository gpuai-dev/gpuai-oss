package provider

import "testing"

func TestMiBToGB(t *testing.T) {
	cases := []struct {
		name string
		mib  int
		want int
	}{
		// Exact multiples — the only shape the old integer division handled.
		{"exact 232GB", 237568, 232},
		{"exact 1856GB", 1900544, 1856},
		// Non-multiples: truncation under-reported these, rounding does not.
		// 16376 MiB is the RTX A4000 figure that advertised as 15GB.
		{"a4000 vram shape", 16376, 16},
		{"just under a boundary", 1023, 1},
		{"just over a boundary", 1025, 1},
		{"rounds up past half", 1600, 2},
		{"rounds down below half", 1500, 1},
		// A missing spec must stay missing, never negative or fabricated.
		{"zero", 0, 0},
		{"negative", -1024, 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := MiBToGB(tt.mib); got != tt.want {
				t.Errorf("MiBToGB(%d) = %d, want %d", tt.mib, got, tt.want)
			}
		})
	}
}

// The boundary where a present upstream value becomes a missing spec: below
// half a GB there is no whole-GB figure to show, so the row renders "-".
func TestMiBToGBSubGigabyte(t *testing.T) {
	for mib, want := range map[int]int{1: 0, 511: 0, 512: 1, 513: 1} {
		if got := MiBToGB(mib); got != want {
			t.Errorf("MiBToGB(%d) = %d, want %d", mib, got, want)
		}
	}
}
