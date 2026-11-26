package katui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

type broker struct {
	options
	client      *kgo.Client
	admin, disk *kadm.Client
}

type partKey struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
}

func (k partKey) String() string { return fmt.Sprintf("%s:%d", k.Topic, k.Partition) }

type partition struct {
	partKey
	Low      *int64  `json:"low"`
	End      *int64  `json:"end"`
	Commit   *int64  `json:"committed,omitempty"`
	Bytes    *int64  `json:"replica_bytes,omitempty"`
	Leader   *int32  `json:"leader,omitempty"`
	Replicas []int32 `json:"replicas,omitempty"`
	ISR      []int32 `json:"isr,omitempty"`
	Owner    string  `json:"owner,omitempty"`
	Client   string  `json:"client,omitempty"`
	Host     string  `json:"host,omitempty"`
	Error    string  `json:"error,omitempty"`
}

func (p partition) lag() *int64 {
	if p.End == nil || p.Commit == nil || *p.End < *p.Commit {
		return nil
	}
	return ptr(*p.End - *p.Commit)
}
func (p partition) retained() *int64 {
	if p.Low == nil || p.End == nil {
		return nil
	}
	return ptr(max(0, *p.End-*p.Low))
}
func ptr[T any](v T) *T { return &v }

type topicSample struct {
	Name       string      `json:"topic"`
	ID         string      `json:"id"`
	Internal   bool        `json:"internal"`
	Partitions []partition `json:"partitions"`
	At         time.Time   `json:"sampled_at"`
	Error      string      `json:"error,omitempty"`
}
type groupSample struct {
	Name       string      `json:"group"`
	State      string      `json:"state"`
	Partitions []partition `json:"partitions"`
	At         time.Time   `json:"sampled_at"`
	Error      string      `json:"error,omitempty"`
}

func (b *broker) metadata(ctx context.Context, names ...string) (kadm.TopicDetails, error) {
	return b.admin.ListTopicsWithInternal(ctx, names...)
}

func listed(offsets kadm.ListedOffsets, p partKey) (*int64, error) {
	row, ok := offsets.Lookup(p.Topic, p.Partition)
	if !ok {
		return nil, fmt.Errorf("%s: offset is unavailable", p)
	}
	if row.Err != nil {
		return nil, row.Err
	}
	if row.Offset < 0 {
		return nil, fmt.Errorf("%s: offset is unknown", p)
	}
	return ptr(row.Offset), nil
}

func (b *broker) topicOffsets(ctx context.Context, metadata kadm.TopicDetails) (map[string]topicSample, error) {
	result := make(map[string]topicSample, len(metadata))
	names := metadata.Names()
	if len(names) == 0 {
		return result, nil
	}
	var low, high kadm.ListedOffsets
	var lowErr, highErr error
	done := make(chan struct{})
	go func() { low, lowErr = b.admin.ListStartOffsets(ctx, names...); close(done) }()
	high, highErr = b.admin.ListEndOffsets(ctx, names...)
	<-done
	at := time.Now()
	for _, t := range metadata {
		row := topicSample{Name: t.Topic, ID: t.ID.String(), Internal: t.IsInternal || strings.HasPrefix(t.Topic, "__"), At: at, Partitions: []partition{}}
		if t.Err != nil {
			row.Error = t.Err.Error()
		}
		for _, p := range t.Partitions.Sorted() {
			entry := partition{partKey: partKey{t.Topic, p.Partition}, Replicas: p.Replicas, ISR: p.ISR}
			if p.Leader >= 0 && p.Err == nil {
				entry.Leader = ptr(p.Leader)
			}
			var e1, e2 error
			entry.Low, e1 = listed(low, entry.partKey)
			entry.End, e2 = listed(high, entry.partKey)
			if err := errors.Join(p.Err, e1, e2); err != nil {
				entry.Error = err.Error()
			}
			row.Partitions = append(row.Partitions, entry)
		}
		result[t.Topic] = row
	}
	return result, errors.Join(lowErr, highErr)
}

func (b *broker) sizes(ctx context.Context, metadata kadm.TopicDetails) (map[partKey]int64, error) {
	replicas := map[partKey]map[int32]int64{}
	result := map[partKey]int64{}
	selected := metadata.TopicsSet()
	if len(selected) == 0 {
		return result, nil
	}
	response, err := b.disk.DescribeAllLogDirs(ctx, selected)
	for _, brokerDirs := range response {
		brokerDirs.Each(func(d kadm.DescribedLogDir) {
			if d.Err != nil {
				err = errors.Join(err, d.Err)
				return
			}
			d.Topics.Each(func(p kadm.DescribedLogDirPartition) {
				if p.IsFuture || p.Size < 0 {
					return
				}
				key := partKey{p.Topic, p.Partition}
				if replicas[key] == nil {
					replicas[key] = map[int32]int64{}
				}
				replicas[key][d.Broker] = p.Size
			})
		})
	}
	for _, t := range metadata {
		for id, p := range t.Partitions {
			key := partKey{t.Topic, id}
			total := int64(0)
			complete := p.Err == nil && len(p.Replicas) > 0
			for _, brokerID := range p.Replicas {
				size, ok := replicas[key][brokerID]
				if !ok {
					complete = false
					break
				}
				total += size
			}
			if complete {
				result[key] = total
			}
		}
	}
	return result, err
}

func (b *broker) groupNames(ctx context.Context) ([]string, error) {
	listed, err := b.admin.ListGroups(ctx)
	return sortedKeys(listed), err
}

func (b *broker) groupSnapshots(ctx context.Context, names []string) map[string]groupSample {
	result := make(map[string]groupSample, len(names))
	if len(names) == 0 {
		return result
	}
	descriptions, descErr := b.admin.DescribeGroups(ctx, names...)
	commits := b.admin.FetchManyOffsets(ctx, names...)
	consumerNames := []string{}
	for _, name := range names {
		if d, ok := descriptions[name]; !ok || d.Err != nil {
			consumerNames = append(consumerNames, name)
		}
	}
	modern := kadm.DescribedConsumerGroups{}
	if len(consumerNames) > 0 {
		modern, _ = b.admin.DescribeConsumerGroups(ctx, consumerNames...)
	}
	topics := map[string]bool{}
	for _, name := range names {
		g := groupSample{Name: name, Partitions: []partition{}}
		rows := map[partKey]partition{}
		if d, ok := descriptions[name]; ok && d.Err == nil {
			g.State = d.State
			for _, member := range d.Members {
				if assignment, ok := member.Assigned.AsConsumer(); ok {
					for _, t := range assignment.Topics {
						for _, id := range t.Partitions {
							key := partKey{t.Topic, id}
							rows[key] = partition{partKey: key, Owner: member.MemberID, Client: member.ClientID, Host: member.ClientHost}
						}
					}
				}
			}
		} else if d, ok := modern[name]; ok && d.Err == nil {
			g.State = d.State
			for _, member := range d.Members {
				for topic, partitions := range member.Assignment {
					for id := range partitions {
						key := partKey{topic, id}
						rows[key] = partition{partKey: key, Owner: member.MemberID, Client: member.ClientID, Host: member.ClientHost}
					}
				}
			}
		} else {
			g.Error = "group description unavailable"
			if d.Err != nil {
				g.Error = d.Err.Error()
			} else if descErr != nil {
				g.Error = descErr.Error()
			}
		}
		if f, ok := commits[name]; ok && f.Err == nil {
			f.Fetched.Each(func(c kadm.OffsetResponse) {
				key := partKey{c.Topic, c.Partition}
				p := rows[key]
				p.partKey = key
				if c.Err != nil {
					p.Error = c.Err.Error()
					g.Error = "some committed offsets are unavailable"
				} else if c.At >= 0 {
					p.Commit = ptr(c.At)
				}
				if p.Commit != nil || p.Owner != "" || p.Error != "" {
					rows[key] = p
				}
			})
		} else {
			g.Error = strings.TrimSpace(g.Error + "; committed offsets unavailable")
			if f.Err != nil {
				g.Error += ": " + f.Err.Error()
			}
		}
		for _, p := range rows {
			topics[p.Topic] = true
			g.Partitions = append(g.Partitions, p)
		}
		sortParts(g.Partitions)
		result[name] = g
	}
	var ends kadm.ListedOffsets
	var offsetErr error
	if len(topics) > 0 {
		names := make([]string, 0, len(topics))
		for topic := range topics {
			names = append(names, topic)
		}
		ends, offsetErr = b.admin.ListEndOffsets(ctx, names...)
	}
	at := time.Now()
	for name, g := range result {
		g.At = at
		for i, p := range g.Partitions {
			end, err := listed(ends, p.partKey)
			g.Partitions[i].End = end
			if err != nil {
				g.Partitions[i].Error = err.Error()
				if offsetErr != nil {
					g.Partitions[i].Error = offsetErr.Error()
				}
			}
		}
		result[name] = g
	}
	return result
}

func sortParts(parts []partition) {
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].Topic != parts[j].Topic {
			return parts[i].Topic < parts[j].Topic
		}
		return parts[i].Partition < parts[j].Partition
	})
}

func nonnegative(s string) *int64 {
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil || i < 0 {
		return nil
	}
	return &i
}

func (b *broker) once(ctx context.Context) (any, error) {
	if b.group != "" {
		g := b.groupSnapshots(ctx, []string{b.group})[b.group]
		if g.Error != "" {
			return nil, errors.New(g.Error)
		}
		selected := g.Partitions[:0]
		for _, p := range g.Partitions {
			if (b.topic == "" || b.topic == p.Topic) && (b.partition < 0 || int(p.Partition) == b.partition) {
				selected = append(selected, p)
			}
		}
		g.Partitions = selected
		return g, nil
	}
	if b.topic == "" {
		names, err := b.groupNames(ctx)
		if names == nil {
			names = []string{}
		}
		return names, err
	}
	md, err := b.metadata(ctx, b.topic)
	if err != nil {
		return nil, err
	}
	t, ok := md[b.topic]
	if !ok {
		return nil, errors.New("topic missing from metadata response")
	}
	if t.Err != nil {
		return nil, t.Err
	}
	ts, err := b.topicOffsets(ctx, md)
	if err != nil {
		return nil, err
	}
	result := ts[b.topic]
	if b.partition >= 0 {
		for _, p := range result.Partitions {
			if int(p.Partition) == b.partition {
				result.Partitions = []partition{p}
				return result, nil
			}
		}
		return nil, errors.New("partition not found")
	}
	return result, nil
}
