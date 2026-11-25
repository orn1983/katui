package katui

import "testing"

func TestConfigDurationDisplayAndEdit(t *testing.T) {
	for _, tc := range []struct{ raw, display, edit string }{
		{"0", "0ms", "0ms"},
		{"999", "999ms", "999ms"},
		{"1000", "1s", "1s"},
		{"59999", "59.999s", "59.999s"},
		{"60000", "1m", "1m"},
		{"60001", "≈1m", "60001"},
		{"90000", "1.5m", "1.5m"},
		{"3600000", "1h", "1h"},
		{"5400000", "1.5h", "1.5h"},
		{"86400000", "1d", "1d"},
		{"103680000", "1.2d", "1.2d"},
		{"604800000", "1w", "1w"},
		{"907200000", "1.5w", "1.5w"},
		{"2592000000", "1mo", "1mo"},
		{"3931200000", "≈1.517mo", "3931200000"},
		{"9223372036854775807", "≈3558399705.577mo", "9223372036854775807"},
		{"-1", "Unlimited", "-1"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			const key = "retention.ms"
			if got := humanConfigDuration(key, tc.raw); got != tc.display {
				t.Errorf("display = %q, want %q", got, tc.display)
			}
			edit := durationEditValue(key, tc.raw)
			if edit != tc.edit {
				t.Errorf("edit = %q, want %q", edit, tc.edit)
			}
			if got, err := configValue(key, edit); err != nil || got != tc.raw {
				t.Fatalf("staging unchanged %q returned %q, %v; want %q", edit, got, err, tc.raw)
			}
		})
	}
}

func TestConfigDurationUnitInput(t *testing.T) {
	for input, want := range map[string]string{
		"1.2d": "103680000", "6.5w": "3931200000", "1mo": "2592000000",
		"1mo1m": "2592060000", "1.5mo": "3888000000", "30d": "2592000000",
	} {
		if got, err := configValue("retention.ms", input); err != nil || got != want {
			t.Errorf("parse %q = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, invalid := range []string{"1month", "2mos", "1mo1", "-2ms", "0.0001s", "3558399706mo"} {
		if v, err := configValue("retention.ms", invalid); err == nil {
			t.Errorf("invalid %q accepted as %q", invalid, v)
		}
	}
	if v, err := configValue("local.retention.ms", "-2"); err != nil || v != "-2" {
		t.Errorf("local retention sentinel = %q, %v", v, err)
	}
}

func TestDescribeConfigValue(t *testing.T) {
	for _, tc := range []struct{ key, raw, want string }{
		{"retention.ms", "60001", "≈1m (60001 ms)"},
		{"retention.ms", "-1", "Unlimited (-1)"},
		{"local.retention.ms", "-2", "Use retention.ms (-2)"},
		{"other.ms", "-1", "-1"},
		{"other.ms", "unexpected", `"unexpected"`},
		{"cleanup.policy", "compact", `"compact"`},
	} {
		if got := describeConfigValue(tc.key, tc.raw); got != tc.want {
			t.Errorf("%s %q produced %q, want %q", tc.key, tc.raw, got, tc.want)
		}
	}
}
