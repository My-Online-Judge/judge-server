package main

import (
	"testing"
	"time"
)

func TestMockSlow(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 3 * time.Second, false},
		{"0", 0, false},
		{"0.3", 300 * time.Millisecond, false},
		{"10", 10 * time.Second, false},
		{"86400", 24 * time.Hour, false},
		{"-1", 0, true},
		{"86401", 0, true},
		{"abc", 0, true},
		{"3s", 0, true},
		{"NaN", 0, true},
		{"Inf", 0, true},
		{"1e400", 0, true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := mockSlow(func(name string) string {
				if name != "MOCK_SLOW_SECONDS" {
					t.Errorf("read %s, want only MOCK_SLOW_SECONDS", name)
				}
				return tc.in
			})
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("mockSlow(%q) = %v, %v; want %v, error %v", tc.in, got, err, tc.want, tc.wantErr)
			}
		})
	}
}
