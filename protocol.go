package piko

import (
	"fmt"
	"strings"
)

type Protocol int

const (
	ProtocolAuto Protocol = iota
	ProtocolHTTP1
	ProtocolHTTP2
	ProtocolH2C
)

func (p Protocol) String() string {
	switch p {
	case ProtocolAuto:
		return "auto"
	case ProtocolHTTP1:
		return "http1"
	case ProtocolHTTP2:
		return "http2"
	case ProtocolH2C:
		return "h2c"
	default:
		return "unknown"
	}
}

func ParseProtocol(value string) (Protocol, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.ReplaceAll(normalized, "_", "")
	normalized = strings.ReplaceAll(normalized, "/", "")
	normalized = strings.ReplaceAll(normalized, ".", "")

	switch normalized {
	case "", "auto", "default":
		return ProtocolAuto, nil
	case "1", "10", "11", "h1", "h10", "h11", "http1", "http10", "http11":
		return ProtocolHTTP1, nil
	case "2", "h2", "http2":
		return ProtocolHTTP2, nil
	case "h2c", "http2c", "cleartexth2", "unencryptedhttp2":
		return ProtocolH2C, nil
	default:
		return ProtocolAuto, fmt.Errorf("unknown protocol %q (use auto, http1, http2, or h2c)", value)
	}
}
