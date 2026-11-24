package katui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kmsg"
)

const (
	opCreate      = "Create topic"
	opConfig      = "Configure topic"
	opDeleteTopic = "Delete topic"
	opReset       = "Reset offsets"
	opRebalance   = "Request rebalance"
	opDeleteGroup = "Delete consumer group"
)

var topicName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,249}$`)
var durationPart = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(ms|s|m|h|d|w)`)

func configValue(key, value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasSuffix(key, ".ms") {
		return value, nil
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		if n >= -1 {
			return strconv.FormatInt(n, 10), nil
		}
		return "", errors.New("milliseconds must be at least -1")
	}
	if len(value) > 256 {
		return "", errors.New("duration is too long")
	}
	units := map[string]int64{"ms": 1, "s": 1000, "m": 60000, "h": 3600000, "d": 86400000, "w": 604800000}
	total := new(big.Rat)
	input := value
	for input != "" {
		match := durationPart.FindStringSubmatch(input)
		if match == nil {
			return "", errors.New("use integer milliseconds or a duration such as 1h30m")
		}
		n, _ := new(big.Rat).SetString(match[1])
		n.Mul(n, new(big.Rat).SetInt64(units[match[2]]))
		total.Add(total, n)
		input = input[len(match[0]):]
	}
	if value == "" || !total.IsInt() || !total.Num().IsInt64() {
		return "", errors.New("duration must resolve to whole signed 64-bit milliseconds")
	}
	return total.Num().String(), nil
}

type setting struct {
	value, source                  string
	sensitive, readOnly, available bool
}
type topicIdentity struct {
	id         kadm.TopicID
	layout     string
	partitions int
}
type memberIdentity struct{ id, instance string }
type groupIdentity struct {
	state, protocol, kind, assignor string
	coordinator                     int32
	members                         []memberIdentity
}
type offsetChange struct {
	key                    partKey
	old, target, low, high int64
	fallback               bool
}
type adminPlan struct {
	operation, target, cluster, review string
	partitions                         int32
	replicas                           int16
	settings                           map[string]*string
	oldSettings                        map[string]setting
	topics                             map[string]topicIdentity
	group                              groupIdentity
	offsets                            map[partKey]int64
	changes                            []offsetChange
}
type adminInput struct {
	operation, target string
	partitions        int32
	replicas          int16
	settings          map[string]*string
	selected          []partKey
	mode              int
	value             string
}

func kafkaError(code int16, message *string) error {
	err := kerr.ErrorForCode(code)
	if err != nil && message != nil && *message != "" {
		return fmt.Errorf("%w: %s", err, *message)
	}
	return err
}

func (b *broker) identity(ctx context.Context, name string) (topicIdentity, error) {
	// Reviews must observe the broker's current topic ID, even when the
	// monitoring client's metadata cache still has the previous topic.
	req := kmsg.NewPtrMetadataRequest()
	req.Topics = []kmsg.MetadataRequestTopic{{Topic: &name}}
	md, err := req.RequestWith(ctx, b.client)
	if err != nil {
		return topicIdentity{}, err
	}
	for _, t := range md.Topics {
		if t.Topic == nil || *t.Topic != name {
			continue
		}
		if err := kerr.ErrorForCode(t.ErrorCode); err != nil {
			return topicIdentity{}, err
		}
		if t.TopicID == ([16]byte{}) || len(t.Partitions) == 0 {
			return topicIdentity{}, errors.New("Kafka did not provide a usable topic ID and partition layout")
		}
		sort.Slice(t.Partitions, func(i, j int) bool { return t.Partitions[i].Partition < t.Partitions[j].Partition })
		var layout strings.Builder
		for _, p := range t.Partitions {
			if err := kerr.ErrorForCode(p.ErrorCode); err != nil {
				return topicIdentity{}, err
			}
			fmt.Fprintf(&layout, "%d=%v;", p.Partition, p.Replicas)
		}
		return topicIdentity{t.TopicID, layout.String(), len(t.Partitions)}, nil
	}
	return topicIdentity{}, fmt.Errorf("%s is missing from the metadata response", name)
}

func (b *broker) settings(ctx context.Context, name string) (map[string]setting, error) {
	req := kmsg.NewPtrDescribeConfigsRequest()
	req.IncludeSynonyms = true
	req.Resources = []kmsg.DescribeConfigsRequestResource{{ResourceType: kmsg.ConfigResourceTypeTopic, ResourceName: name}}
	response, err := req.RequestWith(ctx, b.client)
	if err != nil {
		return nil, err
	}
	for _, r := range response.Resources {
		if r.ResourceName != name {
			continue
		}
		if err := kafkaError(r.ErrorCode, r.ErrorMessage); err != nil {
			return nil, err
		}
		result := map[string]setting{}
		for _, s := range r.Configs {
			value := ""
			if s.Value != nil && !s.IsSensitive {
				value = *s.Value
			}
			result[s.Name] = setting{value: value, source: s.Source.String(), sensitive: s.IsSensitive, readOnly: s.ReadOnly, available: s.Value != nil && !s.IsSensitive}
		}
		return result, nil
	}
	return nil, errors.New("Kafka omitted the requested configuration")
}
func modifiable(key string, s setting) error {
	if !s.available || s.sensitive || s.readOnly {
		return fmt.Errorf("%q is unavailable, sensitive, or read-only", key)
	}
	return nil
}
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for name := range values {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

func (b *broker) metadataBroker(ctx context.Context) (string, int32, error) {
	req := kmsg.NewPtrMetadataRequest()
	req.Topics = []kmsg.MetadataRequestTopic{}
	md, err := req.RequestWith(ctx, b.client)
	if err != nil {
		return "", -1, err
	}
	if len(md.Brokers) == 0 {
		return "", -1, errors.New("no brokers advertised")
	}
	cluster := ""
	if md.ClusterID != nil {
		cluster = *md.ClusterID
	}
	for _, broker := range md.Brokers {
		if broker.NodeID == md.ControllerID {
			return cluster, md.ControllerID, nil
		}
	}
	return cluster, md.Brokers[0].NodeID, nil
}

func (b *broker) supported(ctx context.Context, id int32, key, minimum int16) error {
	request := kmsg.NewPtrApiVersionsRequest()
	request.ClientSoftwareName = "katui"
	request.ClientSoftwareVersion = version
	raw, err := b.client.Broker(int(id)).Request(ctx, request)
	if err != nil {
		return err
	}
	response := raw.(*kmsg.ApiVersionsResponse)
	if err = kerr.ErrorForCode(response.ErrorCode); err != nil {
		return err
	}
	for _, api := range response.ApiKeys {
		if api.ApiKey == key && api.MaxVersion >= minimum {
			return nil
		}
	}
	return fmt.Errorf("broker %d does not support Kafka API %d version %d", id, key, minimum)
}

func (b *broker) create(ctx context.Context, id int32, p adminPlan, validate bool) error {
	req := kmsg.NewPtrCreateTopicsRequest()
	req.TimeoutMillis = int32(min(math.MaxInt32, max(1, b.timeout.Milliseconds())))
	req.ValidateOnly = validate
	topic := kmsg.NewCreateTopicsRequestTopic()
	topic.Topic = p.target
	topic.NumPartitions = p.partitions
	topic.ReplicationFactor = p.replicas
	for _, key := range sortedKeys(p.settings) {
		topic.Configs = append(topic.Configs, kmsg.CreateTopicsRequestTopicConfig{Name: key, Value: p.settings[key]})
	}
	req.Topics = []kmsg.CreateTopicsRequestTopic{topic}
	// Version 0 has no ValidateOnly field.
	if validate {
		if err := b.supported(ctx, id, req.Key(), 1); err != nil {
			return err
		}
	}
	raw, err := b.client.Broker(int(id)).Request(ctx, req)
	if err != nil {
		return err
	}
	for _, r := range raw.(*kmsg.CreateTopicsResponse).Topics {
		if r.Topic == p.target {
			return kafkaError(r.ErrorCode, r.ErrorMessage)
		}
	}
	return errors.New("Kafka omitted the topic from its create response")
}

func (b *broker) configure(ctx context.Context, id int32, p adminPlan, validate bool) error {
	req := kmsg.NewPtrIncrementalAlterConfigsRequest()
	req.ValidateOnly = validate
	resource := kmsg.IncrementalAlterConfigsRequestResource{ResourceType: kmsg.ConfigResourceTypeTopic, ResourceName: p.target}
	for _, key := range sortedKeys(p.settings) {
		value := p.settings[key]
		op := kmsg.IncrementalAlterConfigOpSet
		if value == nil {
			op = kmsg.IncrementalAlterConfigOpDelete
		}
		resource.Configs = append(resource.Configs, kmsg.IncrementalAlterConfigsRequestResourceConfig{Name: key, Op: op, Value: value})
	}
	req.Resources = []kmsg.IncrementalAlterConfigsRequestResource{resource}
	raw, err := b.client.Broker(int(id)).Request(ctx, req)
	if err != nil {
		return err
	}
	for _, r := range raw.(*kmsg.IncrementalAlterConfigsResponse).Resources {
		if r.ResourceName == p.target {
			return kafkaError(r.ErrorCode, r.ErrorMessage)
		}
	}
	return errors.New("Kafka omitted the configuration result")
}

func (b *broker) currentGroup(ctx context.Context, name string) (groupIdentity, error) {
	found := b.admin.FindGroupCoordinators(ctx, name)
	coordinator, ok := found[name]
	if !ok {
		return groupIdentity{}, errors.New("group coordinator is unavailable")
	}
	if coordinator.Err != nil {
		return groupIdentity{}, coordinator.Err
	}
	// Fetch the type explicitly because DescribeGroups' protocol field alone
	// does not distinguish classic from the newer consumer group protocol.
	raw, err := b.client.Broker(int(coordinator.NodeID)).Request(ctx, kmsg.NewPtrListGroupsRequest())
	if err != nil {
		return groupIdentity{}, err
	}
	list := raw.(*kmsg.ListGroupsResponse)
	if err = kerr.ErrorForCode(list.ErrorCode); err != nil {
		return groupIdentity{}, err
	}
	group := groupIdentity{coordinator: coordinator.NodeID}
	foundGroup := false
	for _, g := range list.Groups {
		if g.Group == name {
			foundGroup = true
			group.kind = strings.ToLower(g.GroupType)
			if list.Version < 5 {
				group.kind = "classic"
			}
			break
		}
	}
	if !foundGroup {
		return groupIdentity{}, errors.New("group was absent from its coordinator's group list")
	}
	switch group.kind {
	case "consumer":
		rs, err := b.admin.DescribeConsumerGroups(ctx, name)
		if err != nil {
			return groupIdentity{}, err
		}
		d, ok := rs[name]
		if !ok {
			return groupIdentity{}, errors.New("consumer group description is missing")
		}
		if d.Err != nil {
			return groupIdentity{}, d.Err
		}
		group.state = d.State
		group.protocol = "consumer"
		for _, m := range d.Members {
			group.members = append(group.members, memberIdentity{id: m.MemberID})
		}
	case "classic":
		rs, err := b.admin.DescribeGroups(ctx, name)
		if err != nil {
			return groupIdentity{}, err
		}
		d, err := rs.On(name, nil)
		if err != nil {
			return groupIdentity{}, err
		}
		if d.Err != nil {
			return groupIdentity{}, d.Err
		}
		if d.Coordinator.NodeID != coordinator.NodeID {
			return groupIdentity{}, errors.New("group coordinator changed during observation")
		}
		group.state = d.State
		group.protocol = d.ProtocolType
		group.assignor = d.Protocol
		for _, m := range d.Members {
			instance := ""
			if m.InstanceID != nil {
				instance = *m.InstanceID
			}
			group.members = append(group.members, memberIdentity{m.MemberID, instance})
		}
	default:
		return groupIdentity{}, fmt.Errorf("unsupported group type %q", group.kind)
	}
	slices.SortFunc(group.members, func(a, b memberIdentity) int {
		if n := strings.Compare(a.id, b.id); n != 0 {
			return n
		}
		return strings.Compare(a.instance, b.instance)
	})
	return group, nil
}

func (g groupIdentity) mustBeEmpty() error {
	if len(g.members) != 0 || !strings.EqualFold(g.state, "EMPTY") {
		return fmt.Errorf("group must be EMPTY with no members (state %q, %d members)", g.state, len(g.members))
	}
	return nil
}
func (b *broker) storedOffsets(ctx context.Context, group string) (map[partKey]int64, error) {
	commits, err := b.admin.FetchOffsets(ctx, group)
	if err != nil {
		return nil, err
	}
	if err = commits.Error(); err != nil {
		return nil, err
	}
	offsets := map[partKey]int64{}
	commits.Each(func(p kadm.OffsetResponse) {
		if p.At >= 0 {
			offsets[partKey{p.Topic, p.Partition}] = p.At
		}
	})
	return offsets, nil
}

func (b *broker) timestampOffsets(ctx context.Context, selected []partKey, ms int64) (map[partKey]int64, error) {
	req := kmsg.NewPtrListOffsetsRequest()
	grouped := map[string][]kmsg.ListOffsetsRequestTopicPartition{}
	for _, p := range selected {
		partition := kmsg.NewListOffsetsRequestTopicPartition()
		partition.Partition = p.Partition
		partition.Timestamp = ms
		grouped[p.Topic] = append(grouped[p.Topic], partition)
	}
	for _, topic := range sortedKeys(grouped) {
		req.Topics = append(req.Topics, kmsg.ListOffsetsRequestTopic{Topic: topic, Partitions: grouped[topic]})
	}
	result := map[partKey]int64{}
	for _, response := range b.client.RequestSharded(ctx, req) {
		if response.Err != nil {
			return nil, response.Err
		}
		for _, t := range response.Resp.(*kmsg.ListOffsetsResponse).Topics {
			for _, p := range t.Partitions {
				if err := kerr.ErrorForCode(p.ErrorCode); err != nil {
					return nil, err
				}
				result[partKey{t.Topic, p.Partition}] = p.Offset
			}
		}
	}
	for _, key := range selected {
		if _, ok := result[key]; !ok {
			return nil, fmt.Errorf("Kafka omitted the timestamp offset for %s", key)
		}
	}
	return result, nil
}

func (b *broker) prepareAdmin(ctx context.Context, in adminInput) (adminPlan, error) {
	p := adminPlan{operation: in.operation, target: in.target, partitions: in.partitions, replicas: in.replicas, settings: in.settings, topics: map[string]topicIdentity{},
		oldSettings: map[string]setting{}}
	cluster, brokerID, err := b.metadataBroker(ctx)
	if err != nil {
		return p, err
	}
	p.cluster = cluster
	var preview strings.Builder
	fmt.Fprintf(&preview, "Cluster  %s", b.cluster)
	if p.cluster != "" {
		fmt.Fprintf(&preview, " (%s)", p.cluster)
	}
	fmt.Fprintf(&preview, "\nAction   %s\nTarget   %s\n\n", p.operation, p.target)
	switch p.operation {
	case opCreate:
		if !topicName.MatchString(p.target) || p.target == "." || p.target == ".." {
			return p, errors.New("invalid Kafka topic name")
		}
		if p.partitions < 1 || p.replicas < 1 {
			return p, errors.New("partitions and replication factor must be positive")
		}
		if err = b.create(ctx, brokerID, p, true); err != nil {
			return p, err
		}
		fmt.Fprintf(&preview, "Partitions %d · replication factor %d\n", p.partitions, p.replicas)
		if len(p.settings) == 0 {
			preview.WriteString("Settings use broker defaults.\n")
		}
		for _, key := range sortedKeys(p.settings) {
			fmt.Fprintf(&preview, "%s = %q\n", key, *p.settings[key])
		}
	case opConfig, opDeleteTopic:
		identity, err := b.identity(ctx, p.target)
		if err != nil {
			return p, err
		}
		p.topics[p.target] = identity
		fmt.Fprintf(&preview, "Topic ID %s · %d partitions\n", identity.id, identity.partitions)
		if p.operation == opDeleteTopic {
			if err = b.supported(ctx, brokerID, kmsg.NewPtrDeleteTopicsRequest().Key(), 6); err != nil {
				return p, err
			}
			preview.WriteString("Deleting this topic removes its retained records. Kafka may complete deletion asynchronously.\n")
			break
		}
		if len(p.settings) == 0 {
			return p, errors.New("stage at least one configuration change")
		}
		current, err := b.settings(ctx, p.target)
		if err != nil {
			return p, err
		}
		for _, key := range sortedKeys(p.settings) {
			old := current[key]
			if err := modifiable(key, old); err != nil {
				return p, err
			}
			p.oldSettings[key] = old
			target := "inherit broker default"
			if p.settings[key] != nil {
				target = strconv.Quote(*p.settings[key])
			}
			fmt.Fprintf(&preview, "%s\n  %q (%s) → %s\n", key, old.value, old.source, target)
			if strings.HasPrefix(key, "retention.") || key == "cleanup.policy" {
				preview.WriteString("  This change can remove retained records.\n")
			}
		}
		if err = b.configure(ctx, brokerID, p, true); err != nil {
			return p, err
		}
	case opReset, opDeleteGroup, opRebalance:
		p.group, err = b.currentGroup(ctx, p.target)
		if err != nil {
			return p, err
		}
		if p.operation == opRebalance {
			if p.group.kind != "classic" || p.group.protocol != "consumer" || len(p.group.members) == 0 {
				return p, errors.New("rebalance requires a classic consumer group with members")
			}
			if err = b.supported(ctx, p.group.coordinator, kmsg.NewPtrLeaveGroupRequest().Key(), 3); err != nil {
				return p, err
			}
			preview.WriteString("Group-wide member removal. Running consumers must rejoin, consumption can pause, and static members may need a restart. Committed offsets are unchanged.\n\n")
			for _, m := range p.group.members {
				fmt.Fprintf(&preview, "Member %s", m.id)
				if m.instance != "" {
					fmt.Fprintf(&preview, " · instance %s", m.instance)
				}
				preview.WriteByte('\n')
			}
			break
		}
		if err = p.group.mustBeEmpty(); err != nil {
			return p, err
		}
		p.offsets, err = b.storedOffsets(ctx, p.target)
		if err != nil {
			return p, err
		}
		if p.operation == opDeleteGroup {
			preview.WriteString("Deletes the entire EMPTY consumer group and all its stored offsets across topics. Topic records remain. A restarted application may recreate the group using its configured offset reset policy.\n")
			keys := make([]partKey, 0, len(p.offsets))
			for key := range p.offsets {
				keys = append(keys, key)
			}
			slices.SortFunc(keys, comparePart)
			for _, key := range keys {
				fmt.Fprintf(&preview, "%s  %d\n", key, p.offsets[key])
			}
			break
		}
		if len(in.selected) == 0 {
			return p, errors.New("select at least one committed partition")
		}
		for _, key := range in.selected {
			if _, ok := p.offsets[key]; !ok {
				return p, fmt.Errorf("no current commit for %s", key)
			}
			if _, ok := p.topics[key.Topic]; !ok {
				identity, err := b.identity(ctx, key.Topic)
				if err != nil {
					return p, err
				}
				p.topics[key.Topic] = identity
			}
		}
		names := sortedKeys(p.topics)
		lo, err := b.admin.ListStartOffsets(ctx, names...)
		if err != nil {
			return p, err
		}
		hi, err := b.admin.ListEndOffsets(ctx, names...)
		if err != nil {
			return p, err
		}
		var at map[partKey]int64
		var absolute *int64
		if in.mode == 0 {
			t, e := timestamp(in.value)
			if e != nil {
				return p, e
			}
			if t.UnixMilli() < 0 {
				return p, errors.New("timestamp cannot precede the Unix epoch")
			}
			at, err = b.timestampOffsets(ctx, in.selected, t.UnixMilli())
			if err != nil {
				return p, err
			}
		}
		if in.mode == 1 {
			absolute = nonnegative(in.value)
			if absolute == nil {
				return p, errors.New("absolute offset must be a nonnegative integer")
			}
		}
		preview.WriteString("Offsets identify the next record to read. Moving backward replays records. Moving forward skips them.\n\n")
		for _, name := range names {
			fmt.Fprintf(&preview, "%s · ID %s · %d partitions\n", name, p.topics[name].id, p.topics[name].partitions)
		}
		for _, key := range in.selected {
			low, e := listed(lo, key)
			if e != nil {
				return p, e
			}
			high, e := listed(hi, key)
			if e != nil {
				return p, e
			}
			c := offsetChange{key: key, old: p.offsets[key], low: *low, high: *high, target: *high}
			switch in.mode {
			case 0:
				c.target = at[key]
				if c.target < 0 {
					c.target = *high
					c.fallback = true
				}
			case 1:
				c.target = *absolute
			case 2:
				c.target = *low
			}
			if c.target < *low || c.target > *high {
				return p, fmt.Errorf("%s target %d is outside retained [%d, %d]", key, c.target, *low, *high)
			}
			p.changes = append(p.changes, c)
			suffix := ""
			if c.fallback {
				suffix = " · no later record; using captured end"
			}
			fmt.Fprintf(&preview, "%s  %d → %d  [%d, %d]%s\n", key, c.old, c.target, c.low, c.high, suffix)
		}
	default:
		return p, errors.New("unsupported administration action")
	}
	p.review = preview.String()
	return p, nil
}

func comparePart(a, b partKey) int {
	if n := strings.Compare(a.Topic, b.Topic); n != 0 {
		return n
	}
	return int(a.Partition) - int(b.Partition)
}

func (b *broker) applyAdmin(ctx context.Context, p adminPlan) (bool, error) {
	cluster, brokerID, err := b.metadataBroker(ctx)
	if err != nil {
		return false, err
	}
	if cluster != p.cluster {
		return false, errors.New("cluster identity changed; review again")
	}
	for _, topic := range sortedKeys(p.topics) {
		identity, err := b.identity(ctx, topic)
		if err != nil {
			return false, err
		}
		if identity != p.topics[topic] {
			return false, fmt.Errorf("topic %s identity or partition layout changed; review again", topic)
		}
	}
	switch p.operation {
	case opCreate:
		return true, b.create(ctx, brokerID, p, false)
	case opConfig:
		current, err := b.settings(ctx, p.target)
		if err != nil {
			return false, err
		}
		for key, previous := range p.oldSettings {
			if current[key] != previous {
				return false, fmt.Errorf("setting %s changed; review again", key)
			}
		}
		return true, b.configure(ctx, brokerID, p, false)
	case opDeleteTopic:
		req := kmsg.NewPtrDeleteTopicsRequest()
		req.TimeoutMillis = int32(min(math.MaxInt32, max(1, b.timeout.Milliseconds())))
		identity := p.topics[p.target]
		req.Topics = []kmsg.DeleteTopicsRequestTopic{{TopicID: [16]byte(identity.id)}}
		raw, err := b.client.Broker(int(brokerID)).Request(ctx, req)
		if err != nil {
			return true, err
		}
		for _, r := range raw.(*kmsg.DeleteTopicsResponse).Topics {
			if r.TopicID == [16]byte(identity.id) {
				return true, kafkaError(r.ErrorCode, r.ErrorMessage)
			}
		}
		return true, errors.New("Kafka omitted the reviewed topic ID from its response")
	case opReset, opDeleteGroup, opRebalance:
		group, err := b.currentGroup(ctx, p.target)
		if err != nil {
			return false, err
		}
		if group.kind != p.group.kind || group.protocol != p.group.protocol || group.assignor != p.group.assignor {
			return false, errors.New("group protocol changed; review again")
		}
		if p.operation == opRebalance {
			if !slices.Equal(group.members, p.group.members) {
				return false, errors.New("group membership changed; review again")
			}
			return true, b.removeMembers(ctx, group.coordinator, p)
		}
		if err = group.mustBeEmpty(); err != nil {
			return false, err
		}
		commits, err := b.storedOffsets(ctx, p.target)
		if err != nil {
			return false, err
		}
		if p.operation == opDeleteGroup {
			if !maps.Equal(commits, p.offsets) {
				return false, errors.New("group committed offsets changed; review again")
			}
			req := kmsg.NewPtrDeleteGroupsRequest()
			req.Groups = []string{p.target}
			raw, err := b.client.Broker(int(group.coordinator)).Request(ctx, req)
			if err != nil {
				return true, err
			}
			for _, r := range raw.(*kmsg.DeleteGroupsResponse).Groups {
				if r.Group == p.target {
					return true, kerr.ErrorForCode(r.ErrorCode)
				}
			}
			return true, errors.New("Kafka omitted the group deletion result")
		}
		names := sortedKeys(p.topics)
		lo, err := b.admin.ListStartOffsets(ctx, names...)
		if err != nil {
			return false, err
		}
		hi, err := b.admin.ListEndOffsets(ctx, names...)
		if err != nil {
			return false, err
		}
		for _, c := range p.changes {
			old, ok := commits[c.key]
			if !ok || old != c.old {
				return false, fmt.Errorf("%s committed offset changed; review again", c.key)
			}
			low, e := listed(lo, c.key)
			if e != nil {
				return false, e
			}
			high, e := listed(hi, c.key)
			if e != nil {
				return false, e
			}
			if c.target < *low || c.target > *high {
				return false, fmt.Errorf("%s reviewed offset is outside current [%d, %d]", c.key, *low, *high)
			}
		}
		return true, b.commitReviewed(ctx, group.coordinator, p)
	}
	return false, errors.New("unsupported administration operation")
}

func (b *broker) commitReviewed(ctx context.Context, broker int32, p adminPlan) error {
	req := kmsg.NewPtrOffsetCommitRequest()
	req.Group = p.target
	grouped := map[string][]kmsg.OffsetCommitRequestTopicPartition{}
	for _, c := range p.changes {
		row := kmsg.NewOffsetCommitRequestTopicPartition()
		row.Partition = c.key.Partition
		row.Offset = c.target
		grouped[c.key.Topic] = append(grouped[c.key.Topic], row)
	}
	topicNames := make(map[[16]byte]string, len(grouped))
	for _, topic := range sortedKeys(grouped) {
		id := [16]byte(p.topics[topic].id)
		topicNames[id] = topic
		req.Topics = append(req.Topics, kmsg.OffsetCommitRequestTopic{Topic: topic, TopicID: id, Partitions: grouped[topic]})
	}
	raw, err := b.client.Broker(int(broker)).Request(ctx, req)
	if err != nil {
		return err
	}
	response := raw.(*kmsg.OffsetCommitResponse)
	outcomes := map[partKey]int16{}
	for _, t := range response.Topics {
		name := t.Topic
		if response.Version >= 10 {
			var ok bool
			name, ok = topicNames[t.TopicID]
			if !ok {
				continue
			}
		}
		for _, r := range t.Partitions {
			outcomes[partKey{name, r.Partition}] = r.ErrorCode
		}
	}
	var failures []error
	for _, c := range p.changes {
		code, ok := outcomes[c.key]
		if !ok {
			failures = append(failures, fmt.Errorf("%s missing from response", c.key))
		} else if err = kerr.ErrorForCode(code); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", c.key, err))
		}
	}
	return errors.Join(failures...)
}

func (b *broker) removeMembers(ctx context.Context, broker int32, p adminPlan) error {
	req := kmsg.NewPtrLeaveGroupRequest()
	req.Group = p.target
	for _, m := range p.group.members {
		member := kmsg.NewLeaveGroupRequestMember()
		member.MemberID = m.id
		if m.instance != "" {
			member.InstanceID = ptr(m.instance)
		}
		req.Members = append(req.Members, member)
	}
	if err := b.supported(ctx, broker, req.Key(), 3); err != nil {
		return err
	}
	raw, err := b.client.Broker(int(broker)).Request(ctx, req)
	if err != nil {
		return err
	}
	response := raw.(*kmsg.LeaveGroupResponse)
	if err = kerr.ErrorForCode(response.ErrorCode); err != nil {
		return err
	}
	outcomes := map[memberIdentity]int16{}
	for _, r := range response.Members {
		instance := ""
		if r.InstanceID != nil {
			instance = *r.InstanceID
		}
		outcomes[memberIdentity{r.MemberID, instance}] = r.ErrorCode
	}
	var failures []error
	for _, m := range p.group.members {
		code, ok := outcomes[m]
		if !ok {
			failures = append(failures, fmt.Errorf("member %s missing from Kafka response", m.id))
		} else if err = kerr.ErrorForCode(code); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", m.id, err))
		}
	}
	return errors.Join(failures...)
}
