package main

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
)

// sizeUnits maps a lower-cased unit suffix to its multiplier. A bare number
// is bytes.
var sizeUnits = map[string]int64{
	"":    1,
	"b":   1,
	"kib": 1 << 10,
	"mib": 1 << 20,
	"gib": 1 << 30,
	"kb":  1000,
	"mb":  1000 * 1000,
	"gb":  1000 * 1000 * 1000,
}

// parseSize parses "64MiB"-style sizes: a non-negative integer followed by an
// optional unit (B, KiB, MiB, GiB, KB, MB, GB; case-insensitive; a space
// between number and unit is allowed). Decimal fractions and negative values
// are rejected.
func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, errors.New("size must not be empty")
	}
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("size %q must start with a number", s)
	}
	n, err := strconv.ParseInt(t[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	unit := strings.TrimSpace(t[i:])
	mult, ok := sizeUnits[strings.ToLower(unit)]
	if !ok {
		return 0, fmt.Errorf("size %q has unknown unit %q (use B, KiB, MiB, GiB, KB, MB or GB)", s, unit)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q overflows int64", s)
	}
	return n * mult, nil
}

// parseLogLevel accepts debug, info, warn and error in any letter case (and
// slog's "info+2" offsets, which are harmless).
func parseLogLevel(s string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return 0, fmt.Errorf("log level %q is not one of debug, info, warn, error", s)
	}
	return level, nil
}
