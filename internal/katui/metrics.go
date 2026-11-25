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
	last   *int64
	points []point
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
	history          []ratePoint
	initialLag       *int64
	owner            string
	ownerKnown       bool
	ownerChangeCount int
	ownerChanges     []string
	discontinuities  int
	lastAdvance      time.Time
	identity         string
}

// A chart displays either a topic's incoming rate or a group's commit rate.
func (m *measurement) chartSample(s *series, reset bool) {
	if reset {
		m.history = nil
	}
	rate, known := s.instant()
	m.history = append(m.history, ratePoint{rate, known})
	if len(m.history) > 90 {
		m.history = m.history[len(m.history)-90:]
	}
}

func (m *measurement) observeEnd(at time.Time, end *int64) bool {
	decreased := m.end.decreased(end)
	if decreased {
		m.end = series{}
		m.lastAdvance = time.Time{}
	}
	if end != nil && len(m.end.points) > 0 && *end > m.end.points[len(m.end.points)-1].offset {
		m.lastAdvance = at
	}
	m.end.add(at, end)
	return decreased
}

func (m *measurement) observeTopic(at time.Time, end *int64) {
	decreased := m.observeEnd(at, end)
	if decreased {
		m.discontinuities++
	}
	m.chartSample(&m.end, decreased)
}

func (m *measurement) observe(at time.Time, p partition) {
	de := m.observeEnd(at, p.End)
	dc := m.commit.decreased(p.Commit)
	if dc {
		m.commit = series{}
	}
	if de || dc {
		m.lag = series{}
		m.initialLag = nil
		m.discontinuities++
	}
	m.commit.add(at, p.Commit)
	m.chartSample(&m.commit, dc)
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
	width = max(1, min(width-2, 90))
	if len(values) > width {
		values = values[len(values)-width:]
	}
	peak := 0.0
	hasSamples := false
	for _, v := range values {
		if v.known {
			hasSamples = true
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
	if hasSamples {
		fmt.Fprintf(&out, "Scale %.1f/s  Peak %.1f/s\n", ceiling, peak)
	} else {
		out.WriteString("Waiting for rate samples\n")
	}
	border := strings.Repeat("─", width)
	fmt.Fprintf(&out, "┌%s┐\n", border)
	blocks := []rune(" ▁▂▃▄▅▆▇█")
	const height = 4
	for line := height - 1; line >= 0; line-- {
		out.WriteRune('│')
		out.WriteString(strings.Repeat(" ", width-len(values)))
		for _, v := range values {
			switch {
			case !v.known:
				out.WriteByte(' ')
			case line == 0 && v.rate == 0:
				out.WriteByte('_')
			default:
				steps := int(math.Ceil(max(0, v.rate)/ceiling*height*8)) - line*8
				out.WriteRune(blocks[max(0, min(8, steps))])
			}
		}
		out.WriteString("│\n")
	}
	fmt.Fprintf(&out, "└%s┘\n", border)
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
