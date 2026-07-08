package piko

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"
)

type ConnectionStrategy int

const (
	ConnectionStrategyDefault ConnectionStrategy = iota
	ConnectionStrategySequential
	ConnectionStrategyRoundRobin
	ConnectionStrategyFastest
)

type AddressFamily int

const (
	AddressFamilyAuto AddressFamily = iota
	AddressFamilyIPv4
	AddressFamilyIPv6
	AddressFamilyPreferIPv4
	AddressFamilyPreferIPv6
)

func (s ConnectionStrategy) String() string {
	switch s {
	case ConnectionStrategyDefault:
		return "default"
	case ConnectionStrategySequential:
		return "sequential"
	case ConnectionStrategyRoundRobin:
		return "round-robin"
	case ConnectionStrategyFastest:
		return "fastest"
	default:
		return "unknown"
	}
}

func (f AddressFamily) String() string {
	switch f {
	case AddressFamilyAuto:
		return "auto"
	case AddressFamilyIPv4:
		return "ipv4"
	case AddressFamilyIPv6:
		return "ipv6"
	case AddressFamilyPreferIPv4:
		return "prefer-ipv4"
	case AddressFamilyPreferIPv6:
		return "prefer-ipv6"
	default:
		return "unknown"
	}
}

func ParseConnectionStrategy(value string) (ConnectionStrategy, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "_", "-")

	switch normalized {
	case "", "default", "balanced", "balance":
		return ConnectionStrategyRoundRobin, nil
	case "sequential", "ordered", "first":
		return ConnectionStrategySequential, nil
	case "rr", "roundrobin", "round-robin":
		return ConnectionStrategyRoundRobin, nil
	case "fast", "fastest", "race", "racing":
		return ConnectionStrategyFastest, nil
	default:
		return ConnectionStrategySequential, fmt.Errorf("unknown connection strategy %q (use sequential, round-robin, or fastest)", value)
	}
}

func ParseAddressFamily(value string) (AddressFamily, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "_", "-")

	switch normalized {
	case "", "auto", "dual", "dual-stack", "all", "any":
		return AddressFamilyAuto, nil
	case "4", "v4", "ip4", "ipv4", "tcp4":
		return AddressFamilyIPv4, nil
	case "6", "v6", "ip6", "ipv6", "tcp6":
		return AddressFamilyIPv6, nil
	case "prefer4", "prefer-v4", "prefer-ip4", "prefer-ipv4", "ipv4-preferred", "v4-first":
		return AddressFamilyPreferIPv4, nil
	case "prefer6", "prefer-v6", "prefer-ip6", "prefer-ipv6", "ipv6-preferred", "v6-first":
		return AddressFamilyPreferIPv6, nil
	default:
		return AddressFamilyAuto, fmt.Errorf("unknown IP family %q (use auto, ipv4, ipv6, prefer-ipv4, or prefer-ipv6)", value)
	}
}

type dialIPSelector struct {
	strategy      ConnectionStrategy
	addressFamily AddressFamily
	next          atomic.Uint64
	next4         atomic.Uint64
	next6         atomic.Uint64
	nextAny       atomic.Uint64
}

func newDialIPSelector(strategy ConnectionStrategy, addressFamily AddressFamily) *dialIPSelector {
	return &dialIPSelector{strategy: strategy, addressFamily: addressFamily}
}

func (s *dialIPSelector) order(ips []net.IPAddr) []net.IPAddr {
	if s == nil || len(ips) < 2 {
		return ips
	}

	switch s.strategy {
	case ConnectionStrategyRoundRobin:
		return s.roundRobinOrder(ips)
	case ConnectionStrategySequential:
		return s.sequentialOrder(ips)
	default:
		return s.sequentialOrder(ips)
	}
}

func (s *dialIPSelector) sequentialOrder(ips []net.IPAddr) []net.IPAddr {
	switch s.addressFamily {
	case AddressFamilyPreferIPv4:
		v4, v6, other := splitIPFamilies(ips)
		return appendFamilies(v4, other, v6)
	case AddressFamilyPreferIPv6:
		v4, v6, other := splitIPFamilies(ips)
		return appendFamilies(v6, other, v4)
	default:
		return ips
	}
}

func (s *dialIPSelector) roundRobinOrder(ips []net.IPAddr) []net.IPAddr {
	v4, v6, other := splitIPFamilies(ips)
	v4 = rotateIPs(v4, &s.next4)
	v6 = rotateIPs(v6, &s.next6)
	other = rotateIPs(other, &s.nextAny)

	switch {
	case len(v4) > 0 && len(v6) > 0:
		if s.addressFamily == AddressFamilyPreferIPv4 {
			return appendInterleaved(v4, v6, other)
		}
		if s.addressFamily == AddressFamilyPreferIPv6 {
			return appendInterleaved(v6, v4, other)
		}
		if s.next.Add(1)%2 == 1 {
			return appendInterleaved(v4, v6, other)
		}
		return appendInterleaved(v6, v4, other)
	case len(v4) > 0:
		return append(append([]net.IPAddr{}, v4...), other...)
	case len(v6) > 0:
		return append(append([]net.IPAddr{}, v6...), other...)
	default:
		return other
	}
}

func splitIPFamilies(ips []net.IPAddr) ([]net.IPAddr, []net.IPAddr, []net.IPAddr) {
	var v4, v6, other []net.IPAddr
	for _, ip := range ips {
		switch {
		case ip.IP.To4() != nil:
			v4 = append(v4, ip)
		case ip.IP.To16() != nil:
			v6 = append(v6, ip)
		default:
			other = append(other, ip)
		}
	}
	return v4, v6, other
}

func rotateIPs(ips []net.IPAddr, next *atomic.Uint64) []net.IPAddr {
	if len(ips) < 2 {
		return ips
	}

	start := int(next.Add(1)-1) % len(ips)
	if start == 0 {
		return ips
	}

	ordered := make([]net.IPAddr, 0, len(ips))
	ordered = append(ordered, ips[start:]...)
	ordered = append(ordered, ips[:start]...)
	return ordered
}

func appendFamilies(groups ...[]net.IPAddr) []net.IPAddr {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	ordered := make([]net.IPAddr, 0, total)
	for _, group := range groups {
		ordered = append(ordered, group...)
	}
	return ordered
}

func appendInterleaved(first []net.IPAddr, second []net.IPAddr, rest []net.IPAddr) []net.IPAddr {
	ordered := make([]net.IPAddr, 0, len(first)+len(second)+len(rest))
	for i := 0; i < len(first) || i < len(second); i++ {
		if i < len(first) {
			ordered = append(ordered, first[i])
		}
		if i < len(second) {
			ordered = append(ordered, second[i])
		}
	}
	return append(ordered, rest...)
}
