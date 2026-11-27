package katui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// The whole-value scanner in encoding/json rejects deeply nested containers.
// Decoder.Token uses an explicit delimiter stack and validates the same grammar.
func jsonDepthError(err error) bool {
	var syntaxErr *json.SyntaxError
	return errors.As(err, &syntaxErr) && strings.HasSuffix(syntaxErr.Error(), "exceeded max depth")
}

func jsonEnd(data []byte, offset int64) error {
	if len(bytes.Trim(data[offset:], " \t\n\r")) != 0 {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

// walkJSONTokens expects exactly one complete value. A nil visitor validates it
// without allocating a map or slice for every container.
func walkJSONTokens(ctx context.Context, data []byte, visit func(json.Token)) error {
	dec := json.NewDecoder(contextReader{ctx, bytes.NewReader(data)})
	dec.UseNumber()
	depth := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := dec.Token()
		if err != nil {
			if err == io.EOF && depth > 0 {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if visit != nil {
			visit(token)
		}
		switch token {
		case json.Delim('['), json.Delim('{'):
			depth++
		case json.Delim(']'), json.Delim('}'):
			depth--
		}
		if depth == 0 {
			return jsonEnd(data, dec.InputOffset())
		}
	}
}

func jsonValueContext(ctx context.Context, data []byte) (any, error) {
	dec := json.NewDecoder(contextReader{ctx, bytes.NewReader(data)})
	dec.UseNumber()
	var result any
	if err := dec.Decode(&result); err == nil {
		return result, jsonEnd(data, dec.InputOffset())
	} else if !jsonDepthError(err) {
		return nil, err
	}

	type frame struct {
		object map[string]any
		array  []any
		key    string
		hasKey bool
	}
	var stack []frame
	err := walkJSONTokens(ctx, data, func(token json.Token) {
		var value any = token
		switch token {
		case json.Delim('{'):
			stack = append(stack, frame{object: make(map[string]any)})
			return
		case json.Delim('['):
			stack = append(stack, frame{array: []any{}})
			return
		case json.Delim('}'), json.Delim(']'):
			top := &stack[len(stack)-1]
			if top.object != nil {
				value = top.object
			} else {
				value = top.array
			}
			stack[len(stack)-1] = frame{}
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			result = value
			return
		}
		parent := &stack[len(stack)-1]
		switch {
		case parent.object == nil:
			parent.array = append(parent.array, value)
		case parent.hasKey:
			parent.object[parent.key] = value
			parent.hasKey = false
		default:
			// Decoder.Token only returns strings in the object-key position.
			parent.key = token.(string)
			parent.hasKey = true
		}
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// marshalJSONNode encodes a selected subtree using the same ordering and
// escaping as json.Marshal, without recursing through nested maps and slices.
func marshalJSONNode(ctx context.Context, value any, limit int) ([]byte, error) {
	type frame struct {
		object map[string]any
		keys   []string
		array  []any
		next   int
	}
	var stack []frame
	var out []byte
	appendScalar := func(v any) error {
		if s, ok := v.(string); ok && len(s) > limit-len(out) {
			return errRenderLimit
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if len(encoded) > limit-len(out) {
			return errRenderLimit
		}
		out = append(out, encoded...)
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch v := value.(type) {
		case objectRange:
			out = append(out, '{')
			stack = append(stack, frame{object: v.object, keys: v.keys})
		case map[string]any:
			out = append(out, '{')
			stack = append(stack, frame{object: v, keys: sortedKeys(v)})
		case []any:
			out = append(out, '[')
			stack = append(stack, frame{array: v})
		default:
			if err := appendScalar(v); err != nil {
				return nil, err
			}
		}
		for {
			if len(out) > limit {
				return nil, errRenderLimit
			}
			if len(stack) == 0 {
				return out, nil
			}
			f := &stack[len(stack)-1]
			count, closeDelim := len(f.array), byte(']')
			if f.object != nil {
				count, closeDelim = len(f.keys), '}'
			}
			if f.next == count {
				out = append(out, closeDelim)
				stack = stack[:len(stack)-1]
				continue
			}
			if f.next > 0 {
				out = append(out, ',')
			}
			if f.object != nil {
				key := f.keys[f.next]
				if err := appendScalar(key); err != nil {
					return nil, err
				}
				out = append(out, ':')
				value = f.object[key]
			} else {
				value = f.array[f.next]
			}
			f.next++
			break
		}
	}
}
