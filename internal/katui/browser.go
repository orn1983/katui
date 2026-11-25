package katui

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) freshness(at time.Time, err string, threshold time.Duration) string {
	if err != "" {
		if at.IsZero() {
			return "ERROR"
		}
		return "STALE"
	}
	if at.IsZero() {
		return "LOADING"
	}
	if time.Since(at) > threshold {
		return "STALE"
	}
	return "FRESH"
}

type topicConsumers struct {
	groups     int
	partitions map[int32]bool
	stored     bool
}

type consumerRelationships struct {
	topics          map[string]*topicConsumers
	pending, failed int
}

// Group snapshots change on a poll. Reuse their topic index across table rows,
// status updates, and keystrokes until the next group observation.
func (u *ui) consumersByTopic() *consumerRelationships {
	if u.relationships != nil {
		return u.relationships
	}
	index := &consumerRelationships{topics: make(map[string]*topicConsumers)}
	for _, name := range u.groupNames {
		g, ok := u.groups[name]
		switch {
		case u.groupErrors[name] != "":
			index.failed++
		case !ok || u.cached[name]:
			index.pending++
		}
		assigned := make(map[string]bool)
		for _, p := range g.Partitions {
			r := index.topics[p.Topic]
			if r == nil {
				r = &topicConsumers{}
				index.topics[p.Topic] = r
			}
			r.stored = r.stored || p.Commit != nil
			if p.Owner != "" {
				if r.partitions == nil {
					r.partitions = make(map[int32]bool)
				}
				r.partitions[p.Partition] = true
				if !assigned[p.Topic] {
					r.groups++
					assigned[p.Topic] = true
				}
			}
		}
	}
	u.relationships = index
	return index
}

func (u *ui) relationshipLabel(topic string) string {
	index := u.consumersByTopic()
	value := ""
	if r := index.topics[topic]; r != nil {
		if r.groups > 0 {
			value = fmt.Sprintf("%d groups / %d assigned", r.groups, len(r.partitions))
		} else if r.stored {
			value = "OFFSETS ONLY"
		}
	}
	suffix := ""
	switch {
	case u.relationshipError != "" || index.failed > 0:
		suffix = "UNKNOWN"
	case u.pendingGroups || index.pending > 0 || u.relationshipAt.IsZero():
		suffix = "CHECKING"
	case time.Since(u.relationshipAt) > max(45*time.Second, 2*u.b.interval):
		suffix = "STALE"
	}
	if value == "" && suffix == "" {
		return "NONE"
	}
	if value != "" && suffix != "" {
		return value + " · " + suffix
	}
	return value + suffix
}

func (s isrSummary) overview() (ratio, affected string, color tcell.Color) {
	ratio, affected = "?", "?"
	if s.complete {
		ratio = fmt.Sprintf("%d/%d", s.inSync, s.assigned)
		affected = fmt.Sprint(s.degraded)
	} else if s.degraded > 0 {
		affected = fmt.Sprintf("≥%d", s.degraded)
	}
	if s.stale && (s.complete || s.degraded > 0) {
		ratio += " stale"
	}
	switch {
	case s.critical:
		color = tcell.ColorRed
	case s.degraded > 0:
		color = tcell.ColorYellow
	}
	return
}

func (u *ui) browserRows(loc *location) ([]string, []int, []tableRow) {
	if loc.kind == "messages" {
		return u.recordRows()
	}
	var rows []tableRow
	switch loc.kind {
	case "topics":
		for name, md := range u.metadata {
			internal := md.IsInternal || strings.HasPrefix(name, "__")
			if (internal && !u.showInternal && u.b.topic != name) || !has(name, loc.filter) {
				continue
			}
			t := u.topics[name]
			m := u.metric("", partKey{name, -1})
			var count, bytes int64
			countOK, diskOK := len(t.Partitions) == len(md.Partitions) && len(t.Partitions) > 0, len(md.Partitions) > 0
			for _, p := range t.Partitions {
				if n := p.retained(); n == nil {
					countOK = false
				} else {
					count += *n
				}
			}
			for id := range md.Partitions {
				n, ok := u.sizes[partKey{name, id}]
				diskOK = diskOK && ok
				bytes += n
			}
			var retained, replicaBytes *int64
			if countOK {
				retained = &count
			}
			if diskOK {
				replicaBytes = &bytes
			}
			now, known := m.end.instant()
			var rank int64
			var rateRank float64
			switch loc.sort % 4 {
			case 1:
				rateRank = now
			case 2:
				if countOK {
					rank = count
				}
			case 3:
				if diskOK {
					rank = bytes
				}
			}
			sampleError := t.Error
			if u.errors["offsets"] != "" {
				sampleError = u.errors["offsets"]
			}
			ratio, affected, color := u.topicISR(md).overview()
			rows = append(rows, tableRow{id: name, topic: name, partition: -1, rank: rateRank, rankInteger: rank, alertColor: color, cells: []string{
				name, ratio, affected, u.freshness(t.At, sampleError, max(5*time.Second, 3*u.b.interval)), u.relationshipLabel(name), fmt.Sprint(len(md.Partitions)),
				number(retained), binarySize(replicaBytes), speed(now, known), clock(m.lastAdvance),
			}})
		}
		sortedRows(rows, loc.sort%4 != 0)
		return []string{"Topic", "ISR/Replicas", "Affected P", "Sample", "Consumers", "P", "Messages~", "Disk (replicas)", "Now/s", "Last observed"}, []int{32, 18, 10, 8, 40, 6, 20, 16, 14, 14}, rows
	case "topic":
		t := u.topics[loc.topic]
		parts := t.Partitions
		if len(parts) == 0 {
			for _, md := range u.metadata[loc.topic].Partitions.Sorted() {
				p := partition{partKey: partKey{loc.topic, md.Partition}, Replicas: md.Replicas, ISR: md.ISR}
				if md.Leader >= 0 && md.Err == nil {
					p.Leader = ptr(md.Leader)
				}
				parts = append(parts, p)
			}
		}
		for _, p := range parts {
			if !has(p.partKey.String(), loc.filter) {
				continue
			}
			m := u.metric("", p.partKey)
			now, known := m.end.instant()
			avg, _, avgOK := m.end.average()
			leader := "?"
			if p.Leader != nil {
				leader = fmt.Sprint(*p.Leader)
			}
			var disk *int64
			if n, ok := u.sizes[p.partKey]; ok {
				disk = ptr(n)
			}
			label := fmt.Sprint(p.Partition)
			if p.Error != "" {
				label += " !"
			}
			rows = append(rows, tableRow{id: p.partKey.String(), topic: p.Topic, partition: p.Partition, rank: now, order: int(p.Partition), cells: []string{
				label, number(p.retained()), binarySize(disk), speed(now, known), speed(avg, avgOK), leader,
				fmt.Sprintf("%d/%d", len(p.ISR), len(p.Replicas)), number(p.Low), number(p.End),
			}})
		}
		sortedRows(rows, loc.sort%2 != 0)
		return []string{"Partition", "Messages~", "Disk", "Now/s", "Avg60/s", "Leader", "ISR/RF", "Low", "End"}, []int{9, 20, 16, 14, 14, 8, 8, 20, 20}, rows
	case "groups", "picker":
		for _, name := range u.groupNames {
			if !has(name, loc.filter) {
				continue
			}
			g := u.groups[name]
			var active, idle, total lagBucket
			for _, p := range g.Partitions {
				if loc.topic != "" && p.Topic != loc.topic {
					continue
				}
				total.add(p.lag())
				if p.Owner != "" {
					active.add(p.lag())
				} else {
					idle.add(p.lag())
				}
			}
			sample := u.freshness(g.At, u.groupErrors[name], max(15*time.Second, 2*u.b.interval))
			if u.cached[name] {
				sample = "CACHED"
			}
			row := tableRow{id: name, group: name, topic: loc.topic, partition: -1, rankInteger: max(active.max, idle.max)}
			amin, amax := active.bounds()
			imin, imax := idle.bounds()
			if loc.kind == "picker" {
				relation := "OFFSETS ONLY"
				if total.total == 0 {
					if u.groupErrors[name] == "" {
						continue
					}
					relation = "UNVERIFIED"
				} else if active.total > 0 {
					relation = "ASSIGNED"
				}
				if u.groupErrors[name] != "" {
					relation += " *"
				}
				sum := "?"
				if total.known > 0 {
					sum = integer(total.sum)
					if total.known < total.total {
						sum += "*"
					}
				}
				lo, hi := total.bounds()
				row.rankInteger = total.sum
				if u.width < 104 {
					row.cells = []string{name, integer(total.total), integer(active.total), sum, relation}
				} else {
					row.cells = []string{name, integer(total.total), integer(active.total), sum, lo, hi, relation, age(g.At)}
				}
			} else {
				row.cells = []string{name, amin, amax, imin, imax, sample, age(g.At)}
			}
			rows = append(rows, row)
		}
		sortedRows(rows, loc.sort%2 != 0)
		if loc.kind == "picker" {
			if u.width < 104 {
				return []string{"Group", "P", "Assigned", "Total lag", "Relationship"}, []int{25, 4, 8, 13, 17}, rows
			}
			return []string{"Group", "P", "Assigned", "Total lag", "Min lag", "Max lag", "Relationship", "Age"}, []int{24, 4, 8, 12, 10, 10, 16, 8}, rows
		}
		return []string{"Group", "Active min", "Active max", "Idle min", "Idle max", "Sample", "Age"}, []int{32, 13, 13, 13, 13, 9, 9}, rows
	case "group":
		for i, p := range u.groups[loc.group].Partitions {
			if (loc.topic != "" && p.Topic != loc.topic) || (loc.partition >= 0 && p.Partition != loc.partition) || !has(p.partKey.String(), loc.filter) {
				continue
			}
			m := u.metric(loc.group, p.partKey)
			avg, _, ok := m.commit.average()
			lag := p.lag()
			rank := int64(-1)
			if lag != nil {
				rank = *lag
			}
			label := p.Topic
			if p.Error != "" {
				label += " !"
			}
			rows = append(rows, tableRow{id: p.partKey.String(), topic: p.Topic, group: loc.group, partition: p.Partition, rankInteger: rank, order: i, cells: []string{
				label, fmt.Sprint(p.Partition), number(lag), speed(m.commit.instant()), speed(avg*60, ok), speed(m.end.instant()), m.eta(lag, true),
				percent(m.progress(lag)), fmt.Sprintf("%d", m.ownerChangeCount),
			}})
		}
		sortedRows(rows, loc.sort%2 != 0)
		return []string{"Topic", "P", "Lag", "Now/s", "Avg/min", "In/s", "ETA60s", "Caught up %", "Changes"}, []int{28, 5, 14, 12, 14, 12, 14, 12, 9}, rows
	}
	return nil, nil, nil
}

func (u *ui) partitionDetail(loc *location) string {
	var parts []partition
	if loc.group == "" {
		parts = u.topics[loc.topic].Partitions
	} else {
		parts = u.groups[loc.group].Partitions
	}
	for _, p := range parts {
		if p.Partition != loc.partition || (loc.topic != "" && p.Topic != loc.topic) {
			continue
		}
		m := u.metric(loc.group, p.partKey)
		var body strings.Builder
		fmt.Fprintf(&body, "%s [%d]\n\n", safe(p.Topic, false), p.Partition)
		fmt.Fprintf(&body, "Log end   %s\n", number(p.End))
		if loc.group != "" {
			lag := p.lag()
			avg, _, known := m.commit.average()
			progress, progOK := m.progress(lag)
			fill := 0
			if progOK {
				fill = int(min(100, max(0, progress)) / 5)
			}
			fmt.Fprintf(&body, "Committed %s\nLag       %s\nOwner     %s\nClient    %s\nHost      %s\n\n", number(p.Commit), number(lag), ownerName(p.Owner), safe(p.Client, false), safe(p.Host, false))
			fmt.Fprintf(&body, "Consume %s/s  Avg %s/min  Incoming %s/s\nETA %s\nCaught up [%s%s] %s\nOffset discontinuities %d\n\n",
				speed(m.commit.instant()), speed(avg*60, known), speed(m.end.instant()), m.eta(lag, false), strings.Repeat("█", fill),
				strings.Repeat("░", 20-fill), percent(progress, progOK), m.discontinuities)
			body.WriteString(chart(m.history, u.width-4))
			fmt.Fprintf(&body, "\nOwner changes (%d)\n%s", m.ownerChangeCount, strings.Join(m.ownerChanges, "\n"))
		} else {
			avg, _, known := m.end.average()
			leader := "?"
			if p.Leader != nil {
				leader = fmt.Sprint(*p.Leader)
			}
			var disk *int64
			if n, ok := u.sizes[p.partKey]; ok {
				disk = ptr(n)
			}
			fmt.Fprintf(&body, "Low             %s\nMessages~       %s\nDisk (replicas) %s\nLeader          %s\nReplicas        %v\nIn sync         %v", number(p.Low), number(p.retained()), binarySize(disk), leader, p.Replicas, p.ISR)
			if len(p.ISR) < len(p.Replicas) {
				body.WriteString("  ISR SHORTFALL")
			}
			fmt.Fprintf(&body, "\n\nNow %s/s  Avg60 %s/s\nLast observed %s\nOffset discontinuities %d\n\n",
				speed(m.end.instant()), speed(avg, known), clock(m.lastAdvance), m.discontinuities)
			body.WriteString(chart(m.history, u.width-4))
		}
		if p.Error != "" {
			fmt.Fprintf(&body, "\n%s", safe(p.Error, true))
		}
		return body.String()
	}
	return "Waiting for partition data, or no partition matches this selection."
}

func (u *ui) updateStatus() {
	if u.status == nil || u.workspace == clustersWorkspace {
		return
	}
	loc := u.current()
	var summary string
	switch loc.kind {
	case "messages":
		r := u.readerState
		if r.state == "" {
			r.state = "No reader"
		}
		summary = fmt.Sprintf("%s · scanned %s · matched %s · buffered %d (%s) · evicted %d",
			r.state, integer(r.scanned), integer(r.matched), len(u.records), binarySize(ptr(int64(u.recordBytes))), u.evictions)
		if r.info != "" {
			summary = r.state + " · " + safe(r.info, false)
		}
		if store := u.store; store != nil {
			if missed := store.missed.Load(); missed > 0 {
				summary += fmt.Sprintf("\nWARNING: payload quota reached (%d preview-only) · ", missed)
			} else {
				summary += "\n"
			}
			summary += "Temp " + binarySize(ptr(store.used.Load())) + " / " + binarySize(&store.limit)
		} else {
			summary += "\nTemp storage unused"
		}
		summary += fmt.Sprintf("\nChecked %d/100 (%s / 8 MiB)", len(u.checked), binarySize(ptr(int64(u.checkedBytes))))
		if len(u.readOptions.partitions) > 0 {
			stream := "snapshot"
			if u.readOptions.follow {
				stream = "live"
			}
			summary += fmt.Sprintf(" · P%v %s limit %d", u.readOptions.partitions, stream, u.readOptions.limit)
			if u.readOptions.start == startOffset {
				summary += " · offset " + integer(u.readOptions.offset)
			}
		}
	case "group", "group-detail":
		g := u.groups[loc.group]
		summary = fmt.Sprintf("%s · %s · %s ago", g.State, u.freshness(g.At, u.groupErrors[loc.group], max(5*time.Second, 3*u.b.interval)), age(g.At))
		if u.cached[loc.group] {
			summary = "CACHED · " + summary
		}
		if e := u.groupErrors[loc.group]; e != "" {
			summary += " · " + e
		}
	case "picker":
		relation := u.consumersByTopic()
		summary = fmt.Sprintf("Checked %d · pending %d · failed %d · matching/unverified %d", max(0, len(u.groupNames)-relation.pending-relation.failed), relation.pending, relation.failed, len(u.rows))
		if row := u.selected(); row.group != "" {
			var bucket lagBucket
			g := u.groups[row.group]
			for _, p := range g.Partitions {
				if p.Topic == loc.topic {
					bucket.add(p.lag())
				}
			}
			low, high := bucket.bounds()
			summary += fmt.Sprintf(" · selected %s–%s (%s)", low, high, age(g.At))
		}
	case "topic", "topic-detail":
		t := u.topics[loc.topic]
		summary = fmt.Sprintf("%s · %s ago · %s", u.freshness(t.At, t.Error, max(5*time.Second, 3*u.b.interval)), age(t.At), u.relationshipLabel(loc.topic))
		if t.Error != "" {
			summary += " · " + t.Error
		}
	default:
		summary = fmt.Sprintf("%d rows", len(u.rows))
		request, label := u.monitorRequest(), "topics"
		if loc.kind == "groups" {
			label = "consumer groups"
		}
		if len(u.rows) == 0 {
			switch {
			case u.busy[request] && u.next[request].IsZero():
				summary = "Connecting to Kafka…"
			case u.errors[request] != "":
				summary = "Could not load " + label
			}
		}
	}
	kinds := []string{"metadata", "offsets", "disk", "groups"}
	if u.workspace != 2 {
		for _, kind := range kinds {
			if e := u.errors[kind]; e != "" {
				summary += " · " + kind + ": " + e
			}
		}
	}
	k := keyHint
	bottom := k("?") + " Help & Commands  " + k("q") + " Quit"
	if u.connectionPending() && !(u.workspace == 2 && u.tableZoom) {
		bottom = k("Esc") + " Cancel connection  ·  " + k("q") + " Quit"
	}
	summary = tview.Escape(safe(summary, true))
	if u.workspace != 2 && u.monitorPaused {
		summary = "Connection canceled  ·  " + k("r") + " Retry  ·  " + k("4") + " Clusters"
	}
	if u.tableZoom {
		if u.workspace == 2 {
			bottom = k("z") + " Split view  " + k("i") + " Inspect  ·  " + bottom
		} else {
			bottom = k("Esc") + " " + k("z") + " Restore  ·  " + bottom
		}
	}
	if u.message != "" {
		bottom = tview.Escape(u.message) + "  ·  " + bottom
	}
	text := summary + "\n" + bottom
	if u.status.GetText(false) != text {
		u.status.SetText(text)
	}
}
