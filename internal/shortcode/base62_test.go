package shortcode

import (
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
