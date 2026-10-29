package clicks

import (
	"strings"
	"testing"
)

func TestReferrerHost(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"empty is direct", "", ""},
		{"strips www and path", "https://www.google.com/search?q=x", "google.com"},
		{"lowercases and drops port", "https://News.Example.COM:8443/a", "news.example.com"},
		{"http works", "http://example.org/", "example.org"},
		{"not a URL", "garbage", ""},
		{"non-web scheme", "android-app://com.google.android.gm", ""},
		{"host too long", "https://" + strings.Repeat("a", 101) + ".com/", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReferrerHost(tt.in); got != tt.want {
				t.Errorf("ReferrerHost(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDevice(t *testing.T) {
	tests := []struct{ name, ua, want string }{
		{"empty", "", "other"},
		{"desktop chrome", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36", "desktop"},
		{"iphone", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1", "mobile"},
		{"android", "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36", "mobile"},
		{"googlebot beats mozilla", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "bot"},
		{"curl", "curl/8.4.0", "bot"},
		{"hey", "hey/0.0.1", "bot"},
		{"go client", "Go-http-client/1.1", "bot"},
		{"unknown", "SomethingElse/1.0", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Device(tt.ua); got != tt.want {
				t.Errorf("Device(%q) = %q, want %q", tt.ua, got, tt.want)
			}
		})
	}
}
