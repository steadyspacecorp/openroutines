package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The knowledge working-set window when none is configured.
const DefaultRetention = 30 * 24 * time.Hour

// Accepts days such as 30d or a positive Go duration.
func ParseRetention(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultRetention, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
		return 0, fmt.Errorf("retention %q: use Nd (days) or a duration like 720h", value)
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("retention %q: use Nd (days) or a duration like 720h", value)
	}
	return duration, nil
}
