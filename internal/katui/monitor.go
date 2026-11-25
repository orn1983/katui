package katui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	c := cacheFile{GroupNames: append([]string(nil), u.groupNames...), Groups: map[string]groupSample{}, At: time.Now()}
	for name, g := range u.groups {
		if g.Error == "" {
			c.Groups[name] = g
		}
	}
	if u.cacheBusy {
		return
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
	u.failures = map[string]int{}
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
				u.failures[kind]++
				delay := max(time.Second, interval)
				for i := 1; i < u.failures[kind] && delay < 30*time.Second; i++ {
					delay = min(30*time.Second, delay*2)
				}
				u.next[kind] = time.Now().Add(delay)
			} else {
				delete(u.errors, kind)
				u.failures[kind] = 0
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
	case "group", "group-detail":
		return "group"
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
	request := u.monitorRequest()
	return !u.monitorPaused && (u.busy[request] || u.errors[request] != "")
}

func (u *ui) cancelConnection() {
	if u.workspace == 2 {
		u.stopReader()
	} else {
		u.invalidateMonitor()
		u.pendingGroups = false
		u.monitorPaused = true
	}
	u.focus(u.mainPane())
	u.updateStatus()
}

func (u *ui) poll() {
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
	if loc.group != "" {
		name := loc.group
		u.sample("group", u.b.interval, func(ctx context.Context, _ func(func())) (func(), error) {
			g, ok := u.b.groupSnapshots(ctx, []string{name})[name]
			if !ok {
				return nil, errors.New("group observation unavailable")
			}
			return func() { u.acceptGroup(g) }, stringError(g.Error)
		})
	} else {
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
		if names == nil && nameErr != nil {
			return nil, nameErr
		}
		deliver(func() {
			u.groupNames = append([]string{}, names...)
			u.pendingGroups = true
			u.relationshipAt = time.Now()
			u.relationshipError = errorText(nameErr)
		})
		snapshots := u.b.groupSnapshots(ctx, names)
		return func() {
			u.pendingGroups = false
			for _, name := range names {
				if g, ok := snapshots[name]; ok {
					u.acceptGroup(g)
				} else {
					u.groupErrors[name] = "group check did not finish"
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
			// Observe valid values only, but preserve previous displayed bounds on failure.
			if p.End != nil || p.Error == "" {
				m.observe(t.At, p)
			}
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
			m.observe(t.At, partition{End: ptr(aggregate)})
		} else {
			t.At = old.At
			if t.Error == "" {
				t.Error = "incomplete offset sample"
			}
		}
		u.topics[name] = t
	}
}

func (u *ui) acceptGroup(g groupSample) {
	if g.Error != "" {
		u.groupErrors[g.Name] = g.Error
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

func stringError(message string) error {
	if message == "" {
		return nil
	}
	return errors.New(message)
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
