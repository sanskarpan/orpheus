package ssrfguard

import (
	"context"
	"net"
	"testing"
)

func TestIsDisallowedIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},           // loopback
		{"::1", true},                 // loopback v6
		{"10.0.0.5", true},            // RFC1918
		{"172.16.9.9", true},          // RFC1918
		{"192.168.1.1", true},         // RFC1918
		{"169.254.169.254", true},     // link-local / cloud metadata
		{"fe80::1", true},             // link-local v6
		{"fc00::1", true},             // ULA
		{"0.0.0.0", true},             // unspecified
		{"224.0.0.1", true},           // multicast
		{"8.8.8.8", false},            // public
		{"1.1.1.1", false},            // public
		{"93.184.216.34", false},      // public (example.com)
		{"2606:2800:220:1::1", false}, // public v6
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tc.ip)
			}
			if got := IsDisallowedIP(ip); got != tc.want {
				t.Errorf("IsDisallowedIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

// stubResolver returns fixed IPs so URL validation is hermetic.
type stubResolver map[string][]net.IPAddr

func (s stubResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if ips, ok := s[host]; ok {
		return ips, nil
	}
	return nil, &net.DNSError{Err: "not found", Name: host}
}

func TestValidateURL(t *testing.T) {
	res := stubResolver{
		"evil.example.com": {{IP: net.ParseIP("10.1.2.3")}},                                // resolves internal
		"ok.example.com":   {{IP: net.ParseIP("93.184.216.34")}},                           // resolves public
		"rebind.example":   {{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, // mixed → reject
	}
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"http scheme rejected", "http://ok.example.com/hook", true},
		{"metadata IP literal", "https://169.254.169.254/latest/meta-data/", true},
		{"loopback literal", "https://127.0.0.1/hook", true},
		{"private literal", "https://10.0.0.1/hook", true},
		{"localhost name", "https://localhost/hook", true},
		{"resolves internal", "https://evil.example.com/hook", true},
		{"mixed resolve (one internal)", "https://rebind.example/hook", true},
		{"public IP literal", "https://1.1.1.1/hook", false},
		{"resolves public", "https://ok.example.com/hook", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateURL(context.Background(), tc.url, res)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateURL(%q) err = %v, wantErr = %v", tc.url, err, tc.wantErr)
			}
		})
	}
}
