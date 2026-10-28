package ratelimit

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	trusted, err := ParseTrustedProxies("10.0.0.0/8, 192.168.1.1")
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}

	tests := []struct {
		name    string
		trusted bool // false = no trusted proxies configured at all
		remote  string
		xff     []string // one entry per header line
		want    string
	}{
		{"direct client, no header", true, "203.0.113.9:5555", nil, "203.0.113.9"},
		{"untrusted peer: header ignored", true, "203.0.113.9:5555", []string{"1.2.3.4"}, "203.0.113.9"},
		{"no trusted proxies: header ignored", false, "10.0.0.2:80", []string{"203.0.113.7"}, "10.0.0.2"},
		{"trusted proxy: header used", true, "10.0.0.2:80", []string{"203.0.113.7"}, "203.0.113.7"},
		{"forged left entries ignored", true, "10.0.0.2:80", []string{"1.2.3.4, 203.0.113.7"}, "203.0.113.7"},
		{"chain of trusted proxies", true, "10.0.0.2:80", []string{"203.0.113.7, 10.0.0.1"}, "203.0.113.7"},
		{"header split over two lines", true, "10.0.0.2:80", []string{"1.2.3.4", "203.0.113.7"}, "203.0.113.7"},
		{"single-address trusted entry", true, "192.168.1.1:80", []string{"198.51.100.4"}, "198.51.100.4"},
		{"trusted proxy, no header", true, "10.0.0.2:80", nil, "10.0.0.2"},
		{"malformed header falls back to the proxy", true, "10.0.0.2:80", []string{"garbage"}, "10.0.0.2"},
		{"IPv4-mapped IPv6 is unwrapped", true, "[::ffff:203.0.113.9]:80", nil, "203.0.113.9"},
		{"IPv6 is keyed by /64", true, "[2001:db8:1:2::1]:1234", nil, "2001:db8:1:2::/64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				req.Header.Add("X-Forwarded-For", v)
			}

			list := trusted
			if !tt.trusted {
				list = nil
			}
			if got := ClientIP(req, list); got != tt.want {
				t.Errorf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

// Addresses inside one /64 must share a bucket, or rotating addresses
// would defeat the limit.
func TestClientIP_IPv6SameSubnetSameKey(t *testing.T) {
	key := func(remote string) string {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		return ClientIP(req, nil)
	}

	a := key("[2001:db8:1:2::1]:80")
	b := key("[2001:db8:1:2:ffff::9]:80")
	c := key("[2001:db8:1:3::1]:80")

	if a != b {
		t.Errorf("same /64 gave different keys: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different /64s gave the same key %q", a)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies(" 10.0.0.0/8 , 192.168.1.1 ,, ")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v; want two prefixes and no error", got, err)
	}

	if got, err := ParseTrustedProxies(""); err != nil || got != nil {
		t.Errorf("empty input gave %v, %v; want nil, nil", got, err)
	}

	if _, err := ParseTrustedProxies("not-an-ip"); err == nil {
		t.Error("invalid entry accepted, want an error")
	}
}
