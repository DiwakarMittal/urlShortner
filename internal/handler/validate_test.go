package handler

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateURL is table-driven: each row is one scenario, and one loop
// runs them all. wantErr means "this input should be REJECTED".
func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string // describes the case; shown in -v output and on failure
		input   string
		wantErr bool
	}{
		// Should be accepted
		{"valid https", "https://example.com", false},
		{"valid http", "http://example.com", false},
		{"uppercase scheme", "HTTP://EXAMPLE.COM", false}, // Parse lowercases the scheme
		{"surrounding spaces", "   http://example.com   ", false},

		// Should be rejected
		{"empty", "", true},
		{"only spaces", "   ", true},
		{"no scheme", "example.com", true},
		{"no host", "https://", true},
		{"javascript scheme", "javascript:alert(1)", true},
		{"ftp scheme", "ftp://example.com", true},
		// 20 chars + 2049 'a's is over our 2048 limit
		{"too long", "https://example.com/" + strings.Repeat("a", 2049), true},
	}

	for _, tt := range tests {
		// t.Run makes each case a named subtest, so a failure says exactly
		// which scenario broke, and -v lists them all.
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateURL(tt.input)

			// Compare "did it error?" with "should it have errored?"
			gotErr := err != nil
			if gotErr != tt.wantErr {
				t.Errorf("ValidateURL(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}

			// Rejections must carry our sentinel so the handler can map
			// them to HTTP 400 with errors.Is.
			if tt.wantErr && !errors.Is(err, ErrInvalidURL) {
				t.Errorf("ValidateURL(%q) error = %v, want it to wrap ErrInvalidURL", tt.input, err)
			}
		})
	}
}

// TestValidateURLTrims checks the returned value, not just the error:
// the cleaned (trimmed) URL is what we store.
func TestValidateURLTrims(t *testing.T) {
	got, err := ValidateURL("   http://example.com   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err) // Fatalf stops: nothing left to check
	}

	want := "http://example.com"
	if got != want {
		t.Errorf("ValidateURL returned %q, want %q", got, want)
	}
}
