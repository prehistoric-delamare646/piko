package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func parseHeaders(values []string) http.Header {
	if len(values) == 0 {
		return nil
	}

	headers := make(http.Header)
	for _, value := range values {
		name, headerValue, _ := strings.Cut(value, ":")
		headers.Add(name, strings.TrimSpace(headerValue))
	}
	return headers
}

func parseSize(value string) (int64, error) {
	text := strings.TrimSpace(strings.ToLower(value))
	if text == "" {
		return 0, fmt.Errorf("empty size")
	}

	multiplier := int64(1)
	for _, suffix := range []struct {
		text string
		mul  int64
	}{
		{"kib", 1024},
		{"kb", 1024},
		{"k", 1024},
		{"mib", 1024 * 1024},
		{"mb", 1024 * 1024},
		{"m", 1024 * 1024},
		{"gib", 1024 * 1024 * 1024},
		{"gb", 1024 * 1024 * 1024},
		{"g", 1024 * 1024 * 1024},
	} {
		if strings.HasSuffix(text, suffix.text) {
			multiplier = suffix.mul
			text = strings.TrimSpace(strings.TrimSuffix(text, suffix.text))
			break
		}
	}
	if text == "" {
		return 0, fmt.Errorf("missing number")
	}
	valueFloat, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, err
	}
	if valueFloat <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return int64(valueFloat * float64(multiplier)), nil
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func averageSpeed(bytes int64, elapsed time.Duration) int64 {
	if bytes <= 0 || elapsed <= 0 {
		return 0
	}
	return int64(float64(bytes) / elapsed.Seconds())
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	if d < time.Minute {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}
