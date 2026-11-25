package katui

import (
	"errors"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Kafka stores elapsed milliseconds, so a month here is always 30 days.
var configDurationUnits = []struct {
	suffix string
	ms     int64
}{
	{"ms", 1},
	{"s", 1000},
	{"m", 60_000},
	{"h", 3_600_000},
	{"d", 86_400_000},
	{"w", 604_800_000},
	{"mo", 2_592_000_000},
}

var durationPart = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([a-z]+)`)

func configValue(key, value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasSuffix(key, ".ms") {
		return value, nil
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		if n >= -1 || (n == -2 && key == "local.retention.ms") {
			return strconv.FormatInt(n, 10), nil
		}
		return "", errors.New("unsupported negative millisecond value for " + key)
	}
	if len(value) > 256 {
		return "", errors.New("duration is too long")
	}
	total := new(big.Rat)
	for input := value; input != ""; {
		match := durationPart.FindStringSubmatch(input)
		var ms int64
		if match != nil {
			for _, unit := range configDurationUnits {
				if unit.suffix == match[2] {
					ms = unit.ms
					break
				}
			}
		}
		if ms == 0 {
			return "", errors.New("use integer milliseconds or a duration such as 1h30m, 1.2d or 1mo")
		}
		n, _ := new(big.Rat).SetString(match[1])
		total.Add(total, n.Mul(n, new(big.Rat).SetInt64(ms)))
		input = input[len(match[0]):]
	}
	if value == "" || !total.IsInt() || !total.Num().IsInt64() {
		return "", errors.New("duration must resolve to whole signed 64-bit milliseconds")
	}
	return total.Num().String(), nil
}

func humanConfigDuration(key, raw string) string {
	if !strings.HasSuffix(key, ".ms") {
		return ""
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return ""
	}
	if n < 0 {
		switch {
		case n == -1 && (key == "retention.ms" || key == "local.retention.ms"):
			return "Unlimited"
		case n == -2 && key == "local.retention.ms":
			return "Use retention.ms"
		default:
			return raw
		}
	}
	unit := configDurationUnits[0]
	for _, candidate := range configDurationUnits[1:] {
		if n < candidate.ms {
			break
		}
		unit = candidate
	}
	decimal := big.NewRat(n, unit.ms).FloatString(3)
	label := strings.TrimSuffix(strings.TrimRight(decimal, "0"), ".") + unit.suffix
	// Multiply only the remainder so even a MaxInt64 broker value is safe.
	if (n%unit.ms)*1000%unit.ms != 0 {
		return "≈" + label
	}
	return label
}

func durationEditValue(key, raw string) string {
	label := humanConfigDuration(key, raw)
	if label == "" || strings.HasPrefix(label, "≈") || strings.HasPrefix(raw, "-") {
		return raw
	}
	return label
}

func describeConfigValue(key, raw string) string {
	label := humanConfigDuration(key, raw)
	if label == "" {
		return strconv.Quote(raw)
	}
	if strings.HasPrefix(raw, "-") {
		if label == raw {
			return raw
		}
		return label + " (" + raw + ")"
	}
	return label + " (" + raw + " ms)"
}
