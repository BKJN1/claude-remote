package server

import (
	"net"
	"testing"
)

func TestWildcardRejected(t *testing.T) {
	for _, a := range []string{"0.0.0.0:8443", "[::]:8443", "[::0]:1", "[::ffff:0.0.0.0]:8443"} {
		if _, err := ResolveListen(a, false); err == nil {
			t.Errorf("%s accepted without the dangerous flag", a)
		}
		if _, err := ResolveListen(a, true); err != nil {
			t.Errorf("%s rejected with the dangerous flag: %v", a, err)
		}
	}
}

func TestExplicitLoopbackAllowed(t *testing.T) {
	got, err := ResolveListen("127.0.0.1:9000", false)
	if err != nil || got != "127.0.0.1:9000" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestInvalidListenRejected(t *testing.T) {
	for _, a := range []string{"localhost:80", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:99999", ":8443"} {
		if _, err := ResolveListen(a, false); err == nil {
			t.Errorf("%q accepted", a)
		}
	}
}

func TestFindTailscaleIP(t *testing.T) {
	mk := func(s string) net.Addr {
		ip, n, _ := net.ParseCIDR(s)
		n.IP = ip
		return n
	}
	addrs := []net.Addr{mk("192.168.1.5/24"), mk("100.63.0.1/32"), mk("100.101.102.103/32")}
	ip, ok := findTailscaleIP(addrs)
	if !ok || ip.String() != "100.101.102.103" {
		t.Fatalf("got %v %v", ip, ok)
	}
	if _, ok := findTailscaleIP(addrs[:2]); ok {
		t.Fatal("non-Tailscale address matched (100.63/16 is outside 100.64/10)")
	}
}

func TestOnlyTailscaleNamedInterfaces(t *testing.T) {
	for name, want := range map[string]bool{"Tailscale": true, "tailscale0": true, "Ethernet": false, "Wi-Fi": false, "Mobile Hotspot": false} {
		if isTailscaleIface(name) != want {
			t.Errorf("%q: got %v", name, !want)
		}
	}
}
