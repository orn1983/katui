package katui

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"
)

type observationKey struct {
	group string
	partKey
}

type cacheFile struct {
	GroupNames []string               `json:"group_names"`
	Groups     map[string]groupSample `json:"groups"`
	At         time.Time              `json:"at"`
}

// A watch belongs to this cluster session. Navigating only cancels the
// foreground browser requests, so an in-flight chart sample can finish.
type groupWatch struct {
	next  time.Time
	retry time.Duration
}

type groupPoll struct {
	groups map[string]*groupWatch
	cancel context.CancelFunc
}

func retryDelay(interval, previous time.Duration) time.Duration {
	if previous == 0 {
		return max(time.Second, interval)
	}
	if previous >= 30*time.Second {
		return previous
	}
	return min(30*time.Second, 2*previous)
}

func (u *ui) watchGroup(name string) {
	if u.watchedGroups[name] != nil {
		return
	}
	w := &groupWatch{}
	if g, ok := u.groups[name]; ok && !u.cached[name] && g.Error == "" && u.groupErrors[name] == "" {
		w.next = g.At.Add(u.b.interval)
	}
	u.watchedGroups[name] = w
	u.nextWatchTick.Store(0)
}

func (u *ui) resetGroupPoll() {
	if u.groupPoll != nil {
		u.groupPoll.cancel()
		u.groupPoll = nil
	}
	for _, w := range u.watchedGroups {
		*w = groupWatch{}
	}
	u.nextWatchTick.Store(0)
}

func (u *ui) pollWatchedGroups() {
	if u.monitorPaused || u.groupPoll != nil || len(u.watchedGroups) == 0 || len(u.b.brokers) == 0 {
		return
	}
	now := time.Now()
	if now.UnixNano() < u.nextWatchTick.Load() {
		return
	}
	var names []string
	var earliest time.Time
	for name, w := range u.watchedGroups {
		if !now.Before(w.next) {
			names = append(names, name)
		} else if earliest.IsZero() || earliest.After(w.next) {
			earliest = w.next
		}
	}
	if len(names) == 0 {
		u.nextWatchTick.Store(earliest.UnixNano())
		return
	}
	// The API batches groups. The CLI starts processes per group, so give
	// the longest-waiting group its own timeout and then service the next.
	if u.b.backend == "cli" {
		oldest := names[0]
		for _, name := range names[1:] {
			if u.watchedGroups[name].next.Before(u.watchedGroups[oldest].next) {
				oldest = name
			}
		}
		names = []string{oldest}
	}
	ctx, cancel := context.WithTimeout(u.ctx, u.b.timeout)
	poll := &groupPoll{groups: make(map[string]*groupWatch, len(names)), cancel: cancel}
	for _, name := range names {
		poll.groups[name] = u.watchedGroups[name]
	}
	u.groupPoll = poll
	u.nextWatchTick.Store(0)
	go func() {
		defer cancel()
		snapshots := u.b.groupSnapshots(ctx, names)
		u.post(func() {
			if u.groupPoll != poll {
				return
			}
			u.groupPoll = nil
			for name, w := range poll.groups {
				if u.watchedGroups[name] != w {
					continue // A complete group listing removed this watch.
				}
				g, ok := snapshots[name]
				if !ok {
					g = groupSample{Name: name, At: time.Now(), Error: "group observation unavailable"}
				}
				u.acceptGroup(g)
				if g.Error == "" {
					w.next, w.retry = now.Add(u.b.interval), 0
				} else {
					w.retry = retryDelay(u.b.interval, w.retry)
					w.next = time.Now().Add(w.retry)
				}
			}
			if u.modalDepth == 0 && poll.groups[u.current().group] != nil {
				u.refreshView()
			}
		})
	}()
}

func (u *ui) loadCache() {
	if u.b.noCache {
		return
	}
	path := cachePath(u.b.backend, u.b.cluster)
	data, err := readLimited(path, 16<<20)
	if err != nil {
		return
	}
	var c cacheFile
	if json.Unmarshal(data, &c) != nil || c.At.IsZero() {
		return
	}
	u.groupNames = c.GroupNames
	for name, g := range c.Groups {
		u.groups[name] = g
		u.cached[name] = true
	}
}

func (u *ui) saveCache() {
	if u.b.noCache {
		return
	}
	if u.cacheBusy {
		return
	}
	c := cacheFile{GroupNames: append([]string(nil), u.groupNames...), Groups: map[string]groupSample{}, At: time.Now()}
	for name, g := range u.groups {
		if g.Error == "" {
			c.Groups[name] = g
		}
	}
	u.cacheBusy = true
	backend, cluster := u.b.backend, u.b.cluster
	go func() {
		data, err := json.Marshal(c)
		if err == nil && len(data) <= 16<<20 {
			_ = atomicFile(cachePath(backend, cluster), data)
		}
		u.post(func() { u.cacheBusy = false })
	}()
}

func (u *ui) invalidateMonitor() {
	u.epoch++
	if u.monitorCancel != nil {
		u.monitorCancel()
		u.monitorCancel = nil
	}
	u.busy = map[string]bool{}
	u.next = map[string]time.Time{}
	u.retry = map[string]time.Duration{}
}

func (u *ui) sample(kind string, interval time.Duration, work func(context.Context, func(func())) (func(), error)) {
	now := time.Now()
	if u.busy[kind] || now.Before(u.next[kind]) {
		return
	}
	u.busy[kind] = true
	epoch := u.epoch
	if u.monitorCancel == nil {
		u.monitorCtx, u.monitorCancel = context.WithCancel(u.ctx)
	}
	ctx, cancel := context.WithTimeout(u.monitorCtx, u.b.timeout)
	deliver := func(fn func()) {
		u.post(func() {
			if u.epoch == epoch && u.modalDepth == 0 {
				fn()
				u.refreshView()
			}
		})
	}
	go func() {
		defer cancel()
		apply, err := work(ctx, deliver)
		u.post(func() {
			if u.epoch != epoch {
				return
			}
			u.busy[kind] = false
			if err != nil {
				u.errors[kind] = err.Error()
				if kind == "groups" {
					u.relationshipError = err.Error()
					u.pendingGroups = false
				}
				u.retry[kind] = retryDelay(interval, u.retry[kind])
				u.next[kind] = time.Now().Add(u.retry[kind])
			} else {
				delete(u.errors, kind)
				delete(u.retry, kind)
				u.next[kind] = now.Add(interval)
			}
			if apply != nil {
				apply()
			}
			if u.modalDepth == 0 {
				u.refreshView()
			}
		})
	}()
}

func (u *ui) monitorRequest() string {
	switch u.current().kind {
	case "topics", "topic", "topic-detail":
		return "metadata"
	default:
		return "groups"
	}
}

func (u *ui) connectionPending() bool {
	if u.workspace == clustersWorkspace {
		return false
	}
	if u.workspace == 2 {
		return u.readerState.state == "CONNECTING" || u.readerState.state == "RECONNECTING"
	}
	if name := u.current().group; name != "" {
		g := u.groups[name]
		waiting := u.cached[name] || g.At.IsZero() || g.Error != ""
		return !u.monitorPaused && waiting && (u.groupErrors[name] != "" || u.groupPoll != nil && u.groupPoll.groups[name] != nil)
	}
	request := u.monitorRequest()
	return !u.monitorPaused && (u.busy[request] || u.errors[request] != "")
}

func (u *ui) cancelConnection() {
	if u.workspace == 2 {
		u.stopReader()
	} else {
		u.invalidateMonitor()
		u.resetGroupPoll()
		u.pendingGroups = false
		u.monitorPaused = true
	}
	u.focus(u.mainPane())
	u.updateStatus()
}

func (u *ui) poll() {
	u.pollWatchedGroups()
	if u.monitorPaused || len(u.b.brokers) == 0 || u.modalDepth > 0 || u.workspace >= 2 {
		return
	}
	loc := u.current()
	if u.workspace == 0 && loc.kind != "picker" && loc.kind != "group" && loc.kind != "group-detail" {
		md := u.metadata
		if len(md) == 0 || !time.Now().Before(u.next["metadata"]) {
			u.sample("metadata", 30*time.Second, func(ctx context.Context, _ func(func())) (func(), error) {
				topics, err := u.b.metadata(ctx)
				if err != nil {
					return nil, err
				}
				return func() {
					for name, deadline := range u.deleted {
						if time.Now().Before(deadline) {
							delete(topics, name)
							continue
						}
						if _, exists := topics[name]; exists {
							u.say("Kafka still lists " + name + " after accepting deletion")
						}
						delete(u.deleted, name)
					}
					maps.DeleteFunc(u.metrics, func(k observationKey, _ *measurement) bool {
						_, exists := topics[k.Topic]
						return k.group == "" && !exists
					})
					for name := range u.topics {
						if _, ok := topics[name]; !ok {
							delete(u.topics, name)
						}
					}
					u.metadata = topics
				}, nil
			})
		}
		if len(md) > 0 {
			u.sample("offsets", u.b.interval, func(ctx context.Context, _ func(func())) (func(), error) {
				rows, err := u.b.topicOffsets(ctx, md)
				return func() { u.acceptTopics(rows) }, err
			})
			u.sample("disk", u.b.sizeInterval, func(ctx context.Context, _ func(func())) (func(), error) {
				sizes, err := u.b.sizes(ctx, md)
				return func() {
					if err == nil || len(sizes) > 0 {
						u.sizes = sizes
					}
				}, err
			})
		}
		u.pollGroupList(max(15*time.Second, u.b.interval))
		return
	}
	if loc.group == "" {
		interval := max(5*time.Second, u.b.interval)
		if loc.kind == "picker" {
			interval = max(15*time.Second, u.b.interval)
		}
		u.pollGroupList(interval)
	}
}

func (u *ui) pollGroupList(interval time.Duration) {
	u.sample("groups", interval, func(ctx context.Context, deliver func(func())) (func(), error) {
		names, nameErr := u.b.groupNames(ctx)
		if len(names) == 0 && nameErr != nil {
			return nil, nameErr
		}
		var unwatched []string
		deliver(func() {
			u.updateGroupList(names, nameErr)
			for _, name := range names {
				if u.watchedGroups[name] == nil {
					unwatched = append(unwatched, name)
				}
			}
			u.pendingGroups = true
			u.relationshipAt = time.Now()
			u.relationshipError = errorText(nameErr)
		})
		snapshots := u.b.groupSnapshots(ctx, unwatched)
		return func() {
			u.pendingGroups = false
			for _, name := range unwatched {
				if g, ok := snapshots[name]; ok {
					u.acceptGroup(g)
				} else {
					u.groupErrors[name] = "group check did not finish"
					u.relationships = nil
				}
			}
			u.saveCache()
		}, nameErr
	})
}

func (u *ui) metric(group string, p partKey) *measurement {
	key := observationKey{group, p}
	if u.metrics[key] == nil {
		u.metrics[key] = &measurement{}
	}
	return u.metrics[key]
}

func (u *ui) acceptTopics(rows map[string]topicSample) {
	for name, t := range rows {
		md, ok := u.metadata[name]
		if !ok || md.ID.String() != t.ID {
			continue
		}
		old := u.topics[name]
		if old.ID != t.ID {
			old = topicSample{}
		}
		oldParts := map[partKey]partition{}
		for _, p := range old.Partitions {
			oldParts[p.partKey] = p
		}
		complete := t.Error == ""
		var aggregate int64
		layout := t.ID
		for i, p := range t.Partitions {
			layout += fmt.Sprintf("/%d", p.Partition)
			m := u.metric("", p.partKey)
			if m.identity != t.ID {
				*m = measurement{identity: t.ID}
			}
			if p.Error != "" {
				complete = false
			}
			if p.End != nil {
				aggregate += *p.End
			}
			// Missing observations break the rate series. Keep the last known
			// bounds below so a stale record count can still be displayed.
			m.observeTopic(t.At, p.End)
			if previous, ok := oldParts[p.partKey]; ok && p.Error != "" {
				if p.Low == nil {
					t.Partitions[i].Low = previous.Low
				}
				if p.End == nil {
					t.Partitions[i].End = previous.End
				}
			}
		}
		key := partKey{name, -1}
		m := u.metric("", key)
		if m.identity != layout {
			*m = measurement{identity: layout}
		}
		if complete {
			m.observeTopic(t.At, &aggregate)
		} else {
			m.observeTopic(t.At, nil)
			t.At = old.At
			if t.Error == "" {
				t.Error = "incomplete offset sample"
			}
		}
		u.topics[name] = t
	}
}

// Only a complete broker listing can establish that a group has disappeared.
func (u *ui) updateGroupList(names []string, listErr error) {
	u.relationships = nil
	if listErr != nil {
		all := append(slices.Clone(u.groupNames), names...)
		slices.Sort(all)
		u.groupNames = slices.Compact(all)
		return
	}
	u.groupNames = names
	live := make(map[string]bool, len(names))
	for _, name := range names {
		live[name] = true
	}
	u.removeGroups(func(name string) bool { return !live[name] })
}

func (u *ui) removeGroups(removed func(string) bool) {
	u.relationships = nil
	u.groupNames = slices.DeleteFunc(u.groupNames, removed)
	maps.DeleteFunc(u.groups, func(name string, _ groupSample) bool { return removed(name) })
	maps.DeleteFunc(u.groupErrors, func(name string, _ string) bool { return removed(name) })
	maps.DeleteFunc(u.cached, func(name string, _ bool) bool { return removed(name) })
	maps.DeleteFunc(u.metrics, func(k observationKey, _ *measurement) bool { return k.group != "" && removed(k.group) })
	maps.DeleteFunc(u.watchedGroups, func(name string, _ *groupWatch) bool { return removed(name) })
	// A group can be open in both the Groups workspace and a topic's consumer picker.
	for i, stack := range u.stacks {
		u.stacks[i] = slices.DeleteFunc(stack, func(loc *location) bool {
			return loc.group != "" && removed(loc.group)
		})
	}
	if len(u.watchedGroups) == 0 {
		u.nextWatchTick.Store(0)
	}
}

func (u *ui) acceptGroup(g groupSample) {
	u.relationships = nil
	if g.Error != "" {
		u.groupErrors[g.Name] = g.Error
		for _, p := range u.groups[g.Name].Partitions {
			if m := u.metrics[observationKey{g.Name, p.partKey}]; m != nil {
				m.end.add(g.At, nil)
				m.commit.add(g.At, nil)
				m.lag.add(g.At, nil)
				m.chartSample(&m.commit, false)
			}
		}
		if _, ok := u.groups[g.Name]; !ok {
			u.groups[g.Name] = g
		}
		return
	}
	delete(u.groupErrors, g.Name)
	delete(u.cached, g.Name)
	for _, p := range g.Partitions {
		u.metric(g.Name, p.partKey).observe(g.At, p)
	}
	u.groups[g.Name] = g
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func cachePath(backend, cluster string) string {
	return defaultPath("XDG_CACHE_HOME", ".cache", "monitor-"+shortHash(backend+" "+cluster)+".json")
}

func (u *ui) eraseTopic(name string) {
	u.deleted[name] = time.Now().Add(30 * time.Second)
	// In-flight samplers retain the previous metadata snapshot.
	u.metadata = maps.Clone(u.metadata)
	delete(u.metadata, name)
	delete(u.topics, name)
	for key := range u.sizes {
		if key.Topic == name {
			delete(u.sizes, key)
		}
	}
}
