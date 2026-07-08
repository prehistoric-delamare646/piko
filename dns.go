package piko

import "github.com/UruhaLushia/piko/internal/dns"

// ParseResolver parses DNS resolver strings for Options.Resolver.
func ParseResolver(values ...string) (Resolver, error) {
	return dns.ParseResolver(values...)
}
