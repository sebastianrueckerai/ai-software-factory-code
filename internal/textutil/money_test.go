package textutil

import "testing"

func TestEUR(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{
		{0, "0.00"},
		{0.6, "0.60"},
		{2.675, "2.67"},
		{0.0005, "less than 0.01"},
		{0.0049, "less than 0.01"},
		{0.0099, "0.01"},
		{10 - 9.99, "0.01"},
		{0.01, "0.01"},
		{12, "12.00"},
		{-1.5, "-1.50"},
	} {
		if got := EUR(tc.v); got != tc.want {
			t.Errorf("EUR(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

func TestRate(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{
		{0, "0.0000"},
		{0.0123, "0.0123"},
		{0.00004, "0.0000"},
		{1.5, "1.5000"},
	} {
		if got := Rate(tc.v); got != tc.want {
			t.Errorf("Rate(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}
