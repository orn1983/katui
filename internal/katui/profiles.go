package katui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

const preferenceFileLimit = 1 << 20

type protoProfile struct {
	Path    string   `json:"path"`
	Message string   `json:"message"`
	Framing string   `json:"framing"`
	Include []string `json:"include_paths,omitempty"`
}
type profile struct {
	Mode      string        `json:"mode,omitempty"`
	Columns   []string      `json:"csv_columns,omitempty"`
	Protobuf  *protoProfile `json:"protobuf,omitempty"`
	delimiter string
	header    bool
	schema    *loadedSchema
}

type topicProfiles struct {
	Topics map[string]profile `json:"topics"`
}
type preferenceFile struct {
	Version  int                      `json:"version"`
	Clusters map[string]topicProfiles `json:"clusters"`
}
type preferences struct {
	path, cluster string
	file          preferenceFile
	err           error
}

func loadPreferences(path, cluster string) preferences {
	p := preferences{path: path, cluster: cluster, file: preferenceFile{Version: 1, Clusters: map[string]topicProfiles{}}}
	raw, err := readLimited(path, preferenceFileLimit)
	if errors.Is(err, os.ErrNotExist) {
		return p
	}
	if err == nil {
		var file preferenceFile
		err = json.Unmarshal(raw, &file)
		if err == nil {
			err = file.validate()
		}
		if err == nil {
			p.file = file
		}
	}
	if err != nil {
		p.err = fmt.Errorf("inspector preferences: %w", err)
	}
	return p
}

func (f preferenceFile) validate() error {
	if f.Version != 1 || f.Clusters == nil {
		return errors.New("unsupported inspector preferences format")
	}
	for name, cluster := range f.Clusters {
		for topic, p := range cluster.Topics {
			if err := p.validate(); err != nil {
				return fmt.Errorf("cluster %q topic %q: %w", name, topic, err)
			}
		}
	}
	return nil
}

func (p profile) validate() error {
	if p.Mode != "" && !validMode(p.Mode) {
		return fmt.Errorf("invalid mode %q", p.Mode)
	}
	if err := validateColumns(p.Columns); err != nil {
		return err
	}
	if proto := p.Protobuf; proto != nil && (proto.Path == "" || proto.Message == "" || proto.Framing != "raw" && proto.Framing != "confluent") {
		return errors.New("invalid Protobuf settings")
	}
	return nil
}

func validateColumns(cols []string) error {
	if len(cols) > 64 {
		return errors.New("at most 64 column names are allowed")
	}
	for i, c := range cols {
		if utf8.RuneCountInString(c) > 256 || strings.ContainsAny(c, "\r\n") {
			return fmt.Errorf("column %d must contain at most 256 characters on one line", i+1)
		}
	}
	return nil
}
func (u *ui) profile(topic string) profile {
	if p, ok := u.sessionProfiles[topic]; ok {
		return p
	}
	p := u.prefs.file.Clusters[u.prefs.cluster].Topics[topic]
	if p.Mode == "" {
		p.Mode = "raw"
	}
	if p.delimiter == "" {
		p.delimiter = "Auto"
	}
	return p
}
func (p *preferences) save(topic string, value profile, field string) error {
	current := loadPreferences(p.path, p.cluster)
	if current.err != nil {
		return current.err
	}
	cluster := current.file.Clusters[p.cluster]
	if cluster.Topics == nil {
		cluster.Topics = map[string]profile{}
	}
	persisted := cluster.Topics[topic]
	switch field {
	case "mode":
		persisted.Mode = value.Mode
	case "columns":
		persisted.Columns = value.Columns
	case "protobuf":
		persisted.Protobuf = value.Protobuf
	default:
		return fmt.Errorf("unknown inspector preference %q", field)
	}
	if err := persisted.validate(); err != nil {
		return err
	}
	cluster.Topics[topic] = persisted
	current.file.Clusters[p.cluster] = cluster
	data, err := json.MarshalIndent(current.file, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > preferenceFileLimit {
		return errors.New("1 MiB inspector preference limit reached")
	}
	if err := atomicFile(current.path, data); err != nil {
		return err
	}
	*p = current
	return nil
}

func (u *ui) updateProfile(topic string, p profile, save string) {
	u.sessionProfiles[topic] = p
	if save == "" {
		return
	}
	if err := u.prefs.save(topic, p, save); err != nil {
		u.say("Saving preferences: " + err.Error())
	}
}

func (u *ui) getSchema(ctx context.Context, p profile) (*loadedSchema, error) {
	if p.schema != nil {
		return p.schema, nil
	}
	if p.Protobuf == nil {
		return nil, errors.New("choose a local Protobuf schema and message type with u")
	}
	return loadSchema(ctx, *p.Protobuf)
}
