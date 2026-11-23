package katui

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

type loadedSchema struct {
	messages map[string]protoreflect.MessageDescriptor
	names    []string
	types    *protoregistry.Types
}

func schemaExtension(path string) bool {
	return slices.Contains([]string{".proto", ".pb", ".desc", ".protoset"}, strings.ToLower(filepath.Ext(path)))
}

func loadSchema(ctx context.Context, p protoProfile) (*loadedSchema, error) {
	path, err := filepath.Abs(expandHome(p.Path))
	if err != nil {
		return nil, err
	}
	if !schemaExtension(path) {
		return nil, errors.New("use a .proto, .pb, .desc, or .protoset file")
	}
	data, err := readLimited(path, 8<<20)
	if err != nil {
		return nil, err
	}
	s := &loadedSchema{messages: map[string]protoreflect.MessageDescriptor{}, types: &protoregistry.Types{}}
	if strings.EqualFold(filepath.Ext(path), ".proto") {
		root := filepath.Dir(path)
		folders := []string{root}
		for _, dir := range p.Include {
			dir, err = filepath.Abs(expandHome(dir))
			if err != nil {
				return nil, err
			}
			if !slices.Contains(folders, dir) {
				folders = append(folders, dir)
			}
		}
		resolver := protocompile.ResolverFunc(func(importName string) (protocompile.SearchResult, error) {
			relative := filepath.Clean(filepath.FromSlash(importName))
			if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return protocompile.SearchResult{}, fmt.Errorf("invalid schema import %q", importName)
			}
			var match []byte
			found := false
			for _, folder := range folders {
				candidate, err := readLimited(filepath.Join(folder, relative), 8<<20)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return protocompile.SearchResult{}, err
				}
				if found && !bytes.Equal(match, candidate) {
					return protocompile.SearchResult{}, fmt.Errorf("conflicting copies of %s", importName)
				}
				match, found = candidate, true
			}
			if !found {
				return protocompile.SearchResult{}, os.ErrNotExist
			}
			return protocompile.SearchResult{Source: bytes.NewReader(match)}, nil
		})
		compiler := protocompile.Compiler{Resolver: protocompile.WithStandardImports(resolver)}
		files, err := compiler.Compile(ctx, filepath.Base(path))
		if err != nil {
			return nil, err
		}
		visited := map[string]bool{}
		var visit func(protoreflect.FileDescriptor)
		visit = func(file protoreflect.FileDescriptor) {
			if file == nil || visited[file.Path()] {
				return
			}
			visited[file.Path()] = true
			s.addFile(file)
			for i := 0; i < file.Imports().Len(); i++ {
				visit(file.Imports().Get(i).FileDescriptor)
			}
		}
		for _, f := range files {
			visit(f)
		}
	} else {
		set := &descriptorpb.FileDescriptorSet{}
		if err = proto.Unmarshal(data, set); err != nil {
			return nil, fmt.Errorf("descriptor set: %w", err)
		}
		files, err := protodesc.NewFiles(set)
		if err != nil {
			return nil, err
		}
		files.RangeFiles(func(f protoreflect.FileDescriptor) bool { s.addFile(f); return true })
	}
	for name := range s.messages {
		s.names = append(s.names, name)
	}
	sort.Strings(s.names)
	if len(s.names) == 0 {
		return nil, errors.New("schema declares no selectable message types")
	}
	return s, ctx.Err()
}

func (s *loadedSchema) addFile(f protoreflect.FileDescriptor) {
	var visit func(protoreflect.MessageDescriptors)
	visit = func(messages protoreflect.MessageDescriptors) {
		for i := 0; i < messages.Len(); i++ {
			m := messages.Get(i)
			if !m.IsMapEntry() {
				_ = s.types.RegisterMessage(dynamicpb.NewMessageType(m))
				if !strings.HasPrefix(string(m.FullName()), "google.protobuf.") {
					s.messages[string(m.FullName())] = m
				}
			}
			for i := 0; i < m.Extensions().Len(); i++ {
				_ = s.types.RegisterExtension(dynamicpb.NewExtensionType(m.Extensions().Get(i)))
			}
			visit(m.Messages())
		}
	}
	visit(f.Messages())
	for i := 0; i < f.Extensions().Len(); i++ {
		_ = s.types.RegisterExtension(dynamicpb.NewExtensionType(f.Extensions().Get(i)))
	}
}

func messagePath(d protoreflect.MessageDescriptor) []int {
	path := []int{}
	for {
		path = append(path, d.Index())
		parent, ok := d.Parent().(protoreflect.MessageDescriptor)
		if !ok {
			break
		}
		d = parent
	}
	slices.Reverse(path)
	return path
}
func confluentPayload(data []byte, descriptor protoreflect.MessageDescriptor) ([]byte, error) {
	if len(data) < 6 || data[0] != 0 {
		return nil, errors.New("invalid Confluent magic byte or missing message index")
	}
	data = data[5:]
	n, size := binary.Varint(data)
	if size <= 0 || n < 0 || n > 1000 {
		return nil, errors.New("invalid Confluent message path length")
	}
	data = data[size:]
	path := []int{0}
	if n != 0 {
		path = make([]int, 0, int(n))
		for range n {
			i, used := binary.Varint(data)
			if used <= 0 || i < 0 || i > 1<<30 {
				return nil, errors.New("invalid Confluent message index")
			}
			path = append(path, int(i))
			data = data[used:]
		}
	}
	if !slices.Equal(path, messagePath(descriptor)) {
		return nil, fmt.Errorf("Confluent path %v conflicts with selected type %s (%v)", path, descriptor.FullName(), messagePath(descriptor))
	}
	return data, nil
}
func (s *loadedSchema) decode(profile *protoProfile, value []byte) ([]byte, string, error) {
	d := s.messages[profile.Message]
	if d == nil {
		return nil, "", fmt.Errorf("message %q is unavailable in the selected schema", profile.Message)
	}
	var err error
	if profile.Framing == "confluent" {
		value, err = confluentPayload(value, d)
		if err != nil {
			return nil, "", err
		}
	}
	m := dynamicpb.NewMessage(d)
	if err = (proto.UnmarshalOptions{Resolver: s.types}).Unmarshal(value, m); err != nil {
		return nil, "", err
	}
	recognized := false
	m.Range(func(protoreflect.FieldDescriptor, protoreflect.Value) bool { recognized = true; return false })
	warning := ""
	if len(value) > 0 && !recognized {
		warning = "No recognized fields; check the message type and framing."
	}
	data, err := (protojson.MarshalOptions{Indent: "  ", Resolver: s.types}).Marshal(m)
	return data, warning, err
}
