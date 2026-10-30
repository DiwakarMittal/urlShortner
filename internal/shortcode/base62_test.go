package shortcode

import (
	"math"
	"testing"
)

func TestEncode(t *testing.T) {
	tests := []struct {
		id   int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{61, "Z"},
		{62, "10"},
		{125, "21"},
	}

	for _, tt := range tests {
		got := Encode(tt.id)
		if got != tt.want {
			t.Errorf("Encode(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestDecode(t *testing.T) {
	for _, id := range []int64{0, 1, 61, 62, 125, 3844, 1 << 22, 1 << 40, math.MaxInt64} {
		got, ok := Decode(Encode(id))
		if !ok || got != id {
			t.Errorf("Decode(Encode(%d)) = %d, %v", id, got, ok)
		}
	}
	for _, bad := range []string{"", "ab-c", "a b", "é", "zzzzzzzzzzzz", "ZZZZZZZZZZZ"} {
		if got, ok := Decode(bad); ok {
			t.Errorf("Decode(%q) = %d, true; want false", bad, got)
		}
	}
}
