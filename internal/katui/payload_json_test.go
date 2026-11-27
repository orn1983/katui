package katui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDeepJSONValueAndNodeEncoding(t *testing.T) {
	const pairs = 5100
	prefix, suffix := strings.Repeat(`[{"":`, pairs), strings.Repeat("}]", pairs)
	const leaf = `{"b":[],"a":null,"b":[123456789012345678901.10,"b\u003c",{}]}`
	raw := []byte(prefix + leaf + suffix)
	root, err := jsonValueContext(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	value := root
	for range pairs {
		array, ok := value.([]any)
		if !ok || len(array) != 1 {
			t.Fatal("missing nested array")
		}
		object, ok := array[0].(map[string]any)
		if !ok || len(object) != 1 {
			t.Fatal("missing nested object")
		}
		value = object[""]
	}
	want := map[string]any{"a": nil, "b": []any{json.Number("123456789012345678901.10"), "b<", map[string]any{}}}
	if !reflect.DeepEqual(value, want) {
		t.Errorf("decoded leaf = %#v", value)
	}
	encodedLeaf, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	wantEncoded := []byte(prefix + string(encodedLeaf) + suffix)
	gotEncoded, err := marshalJSONNode(t.Context(), root, len(wantEncoded))
	if err != nil || !bytes.Equal(wantEncoded, gotEncoded) {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := marshalJSONNode(t.Context(), root, len(wantEncoded)-1); !errors.Is(err, errRenderLimit) {
		t.Errorf("bounded node preview error = %v", err)
	}
}

func TestDeepJSONInvalidGrammar(t *testing.T) {
	const depth = 10_001
	left, right := strings.Repeat("[", depth), strings.Repeat("]", depth)
	for name, data := range map[string]string{
		"trailing array comma": left + "0," + right,
		"unterminated object":  left + `{"k":42` + right,
		"missing colon":        left + `{"k"null}` + right,
		"missing value":        left + `{"k":}` + right,
		"invalid number":       left + `01` + right,
		"invalid escape":       left + `"\q"` + right,
		"unclosed arrays":      left + "42",
		"second document":      left + "0" + right + " null",
		"invalid whitespace":   left + "0" + right + "\v",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := jsonValueContext(t.Context(), []byte(data)); err == nil {
				t.Fatal("accepted malformed JSON")
			}
		})
	}
}

func TestJSONTokenWalkCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := walkJSONTokens(ctx, []byte(`[[0]]`), func(json.Token) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("token walk returned %v", err)
	}
}

func TestMarshalJSONNodeObjectRange(t *testing.T) {
	group := objectRange{object: map[string]any{"a": "skip", "b": true, "c": nil}, keys: []string{"b", "c"}}
	const want = `{"b":true,"c":null}`
	got, err := marshalJSONNode(t.Context(), group, len(want))
	if err != nil || string(got) != want {
		t.Errorf("range = %s, err = %v", got, err)
	}
}
