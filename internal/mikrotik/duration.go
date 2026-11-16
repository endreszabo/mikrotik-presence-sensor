package mikrotik

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseDuration parses RouterOS durations such as "1w2d3h4m5s", "5s300ms" or
// "01:02:03".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		var secs float64
		for _, p := range parts {
			n, err := strconv.ParseFloat(p, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			secs = secs*60 + n
		}
		return time.Duration(secs * float64(time.Second)), nil
	}
	units := map[string]time.Duration{
		"w": 7 * 24 * time.Hour, "d": 24 * time.Hour, "h": time.Hour,
		"m": time.Minute, "s": time.Second, "ms": time.Millisecond, "us": time.Microsecond,
	}
	var total time.Duration
	for s != "" {
		i := 0
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
			i++
		}
		j := i
		for j < len(s) && (s[j] < '0' || s[j] > '9') {
			j++
		}
		if i == 0 || i == j {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		n, err := strconv.ParseFloat(s[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		u, ok := units[s[i:j]]
		if !ok {
			return 0, fmt.Errorf("invalid duration unit %q", s[i:j])
		}
		total += time.Duration(n * float64(u))
		s = s[j:]
	}
	return total, nil
}
