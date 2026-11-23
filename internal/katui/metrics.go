package katui

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type point struct {
	at     time.Time
	offset int64
}
type ratePoint struct {
	rate  float64
	known bool
}
type series struct {
	last    *int64
	points  []point
	history []ratePoint
}

func (s *series) add(at time.Time, v *int64) {
	if v == nil {
		s.points = nil
	} else {
		s.points = append(s.points, point{at, *v})
		s.last = v
		// Keep the point immediately before the boundary for interpolation.
		for len(s.points) > 2 && !s.points[1].at.After(at.Add(-time.Minute)) {
			s.points = s.points[1:]
		}
	}
	rate, known := s.instant()
	s.history = append(s.history, ratePoint{rate, known})
	if len(s.history) > 90 {
		s.history = s.history[len(s.history)-90:]
	}
}
func (s *series) instant() (float64, bool) {
	n := len(s.points)
	if n < 2 {
		return 0, false
	}
	a, b := s.points[n-2], s.points[n-1]
	seconds := b.at.Sub(a.at).Seconds()
	if seconds <= 0 {
		return 0, false
	}
	return float64(b.offset-a.offset) / seconds, true
}
func (s *series) average() (rate, coverage float64, known bool) {
	n := len(s.points)
	if n < 2 {
		return
	}
	a, b := s.points[0], s.points[n-1]
	coverage = b.at.Sub(a.at).Seconds()
	if coverage <= 0 {
		return 0, 0, false
	}
	change := float64(b.offset - a.offset)
	if coverage > 60 {
		next := s.points[1]
		elapsed := next.at.Sub(a.at).Seconds()
		if elapsed > 0 {
			change -= float64(next.offset-a.offset) * (coverage - 60) / elapsed
		}
		coverage = 60
	}
	return change / coverage, coverage, true
}
func (s *series) decreased(value *int64) bool {
	return value != nil && s.last != nil && *value < *s.last
}

type measurement struct {
	end, commit, lag series
	initialLag       *int64
	owner            string
	ownerKnown       bool
	ownerChangeCount int
	ownerChanges     []string
	discontinuities  int
	lastAdvance      time.Time
	identity         string
}

func (m *measurement) observe(at time.Time, p partition) {
	de, dc := m.end.decreased(p.End), m.commit.decreased(p.Commit)
	if de {
		m.end = series{}
		m.lastAdvance = time.Time{}
	}
	if dc {
		m.commit = series{}
	}
	if de || dc {
		m.lag = series{}
		m.initialLag = nil
		m.discontinuities++
	}
	if p.End != nil && len(m.end.points) > 0 && *p.End > m.end.points[len(m.end.points)-1].offset {
		m.lastAdvance = at
	}
	m.end.add(at, p.End)
	m.commit.add(at, p.Commit)
	lag := p.lag()
	m.lag.add(at, lag)
	if lag != nil && m.initialLag == nil {
		m.initialLag = lag
	}
	if m.ownerKnown && p.Owner != m.owner {
		m.ownerChangeCount++
		m.ownerChanges = append(m.ownerChanges, fmt.Sprintf("%s  %s → %s", at.Local().Format("01-02 15:04:05"), ownerName(m.owner), ownerName(p.Owner)))
		if len(m.ownerChanges) > 90 {
			m.ownerChanges = m.ownerChanges[1:]
		}
	}
	m.owner = p.Owner
	m.ownerKnown = true
}
func ownerName(owner string) string {
	if owner == "" {
		return "unassigned"
	}
	return safe(owner, false)
}
func (m *measurement) progress(lag *int64) (float64, bool) {
	if lag == nil || m.initialLag == nil || *m.initialLag <= 0 {
		return 0, false
	}
	return 100 * float64(*m.initialLag-*lag) / float64(*m.initialLag), true
}
func (m *measurement) eta(lag *int64, compact bool) string {
	if lag == nil || len(m.lag.points) == 0 {
		return "?"
	}
	if *lag == 0 {
		return "caught up"
	}
	slope, coverage, ok := m.lag.average()
	if !ok || coverage < 60 {
		if compact {
			return fmt.Sprintf("%.0fs/60s", math.Floor(coverage))
		}
		return fmt.Sprintf("warming up (%.0f / 60 seconds)", math.Floor(coverage))
	}
	if slope >= 0 {
		if compact {
			return "--"
		}
		return "not catching up"
	}
	seconds := float64(*lag) / -slope
	if seconds > float64(math.MaxInt64)/float64(time.Second) {
		return fmt.Sprintf("%.0f seconds", seconds)
	}
	return (time.Duration(seconds) * time.Second).String()
}
func chart(values []ratePoint, width int) string {
	width = max(1, min(width, 90))
	if len(values) > width {
		values = values[len(values)-width:]
	}
	peak := 0.0
	for _, v := range values {
		if v.known {
			peak = max(peak, v.rate)
		}
	}
	ceiling := 1.0
	for ceiling < peak {
		base := math.Pow10(int(math.Floor(math.Log10(ceiling))))
		switch {
		case ceiling < 2*base:
			ceiling = 2 * base
		case ceiling < 5*base:
			ceiling = 5 * base
		default:
			ceiling = 10 * base
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Scale %.1f/s  Peak %.1f/s\n", ceiling, peak)
	blocks := []rune(" ▁▂▃▄▅▆▇█")
	const height = 4
	for line := height - 1; line >= 0; line-- {
		out.WriteString(strings.Repeat(" ", width-len(values)))
		for _, v := range values {
			steps := int(math.Ceil(max(0, v.rate)/ceiling*height*8)) - line*8
			if line == 0 && !v.known {
				out.WriteRune('?')
			} else if line == 0 && v.rate == 0 {
				out.WriteRune('·')
			} else {
				out.WriteRune(blocks[max(0, min(8, steps))])
			}
		}
		out.WriteByte('\n')
	}
	return out.String()
}

type lagBucket struct {
	min, max, total, known int64
	sum                    int64
}

func (b *lagBucket) add(v *int64) {
	b.total++
	if v == nil {
		return
	}
	if b.known == 0 {
		b.min = *v
		b.max = *v
	}
	b.min = min(b.min, *v)
	b.max = max(b.max, *v)
	b.known++
	b.sum += *v
}
func (b lagBucket) bounds() (string, string) {
	if b.total == 0 {
		return "--", "--"
	}
	if b.known == 0 {
		return "?", "?"
	}
	suffix := ""
	if b.known < b.total {
		suffix = "*"
	}
	return integer(b.min) + suffix, integer(b.max) + suffix
}
