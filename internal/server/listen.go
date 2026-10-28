package server

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// DefaultPort is used when the listen address is auto-detected.
const DefaultPort = 8443

var tailscaleRange = netip.MustParsePrefix("100.64.0.0/10")

// ResolveListen decides which address to bind. It never silently falls back
// to a wildcard address: without Tailscale (and without an explicit --listen)
// it fails.
func ResolveListen(flag string, dangerous bool) (string, error) {
	if flag != "" {
		return validateListen(flag, dangerous)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	var addrs []net.Addr
	for _, ifc := range ifaces {
		// 100.64.0.0/10 is also the carrier-grade NAT range used by mobile
		// hotspots and ISPs, so the address alone is not proof of Tailscale:
		// require a Tailscale-named interface too.
		if !isTailscaleIface(ifc.Name) {
			continue
		}
		a, err := ifc.Addrs()
		if err == nil {
			addrs = append(addrs, a...)
		}
	}
	if ip, ok := findTailscaleIP(addrs); ok {
		return net.JoinHostPort(ip.String(), strconv.Itoa(DefaultPort)), nil
	}
	return "", errors.New("no Tailscale interface with an address in 100.64.0.0/10 found: is Tailscale running? " +
		"Use --listen IP:PORT to choose explicitly (127.0.0.1:PORT for local testing)")
}

func isTailscaleIface(name string) bool {
	return strings.Contains(strings.ToLower(name), "tailscale")
}

func findTailscaleIP(addrs []net.Addr) (netip.Addr, bool) {
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if ok && tailscaleRange.Contains(ip.Unmap()) {
			return ip.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

func validateListen(addr string, dangerous bool) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid --listen %q: %w", addr, err)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("invalid port in --listen %q", addr)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// Hostnames (even "localhost") are refused: resolution could yield
		// addresses we did not vet.
		return "", fmt.Errorf("--listen host must be an IP address, got %q", host)
	}
	if ip.Unmap().IsUnspecified() && !dangerous {
		return "", errors.New("refusing to listen on a wildcard address (0.0.0.0 or ::): " +
			"it would expose the shell to the whole network; use a Tailscale IP, " +
			"or pass --i-know-this-is-dangerous if you really mean it")
	}
	return addr, nil
}
