package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

type specKind int

const (
	kindAny specKind = iota
	kindObject
	kindArray
	kindString
	kindInt
	kindFloat
	kindBool
)

func (k specKind) String() string {
	switch k {
	case kindObject:
		return "object"
	case kindArray:
		return "array"
	case kindString:
		return "string"
	case kindInt:
		return "integer"
	case kindFloat:
		return "number"
	case kindBool:
		return "boolean"
	}
	return "value"
}

// fieldSpec mirrors the JSON shape of one Go type. It exists because
// encoding/json reports unknown fields and duplicate keys without a usable
// path (and ignores duplicate keys entirely), while operator-facing
// diagnostics need "tiers[1].routes[0]" style locations. The spec is derived
// from the same structs that the typed decode uses, so the rules cannot
// drift from the document types.
type fieldSpec struct {
	kind specKind
	// fields lists the known members of an object (struct). A nil map with
	// kindObject means no members are known.
	fields map[string]*fieldSpec
	// dynamic marks an object whose keys are data (map type) rather than
	// schema members; elem then describes every value.
	dynamic bool
	// elem describes array elements or dynamic map values.
	elem *fieldSpec
}

var jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// specFor derives the structural spec for a document type. Types with their
// own UnmarshalJSON define their own JSON shape, so the scan cannot know it
// and leaves them to the typed decode stage.
func specFor(t reflect.Type) *fieldSpec {
	if t == nil {
		return &fieldSpec{kind: kindAny}
	}
	if t.Implements(jsonUnmarshalerType) || reflect.PointerTo(t).Implements(jsonUnmarshalerType) {
		return &fieldSpec{kind: kindAny}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		fs := &fieldSpec{kind: kindObject, fields: map[string]*fieldSpec{}}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" { // unexported: invisible to encoding/json
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fs.fields[name] = specFor(f.Type)
		}
		return fs
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return &fieldSpec{kind: kindAny}
		}
		return &fieldSpec{kind: kindObject, dynamic: true, elem: specFor(t.Elem())}
	case reflect.Slice, reflect.Array:
		return &fieldSpec{kind: kindArray, elem: specFor(t.Elem())}
	case reflect.String:
		return &fieldSpec{kind: kindString}
	case reflect.Bool:
		return &fieldSpec{kind: kindBool}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &fieldSpec{kind: kindInt}
	case reflect.Float32, reflect.Float64:
		return &fieldSpec{kind: kindFloat}
	}
	return &fieldSpec{kind: kindAny}
}

type structuralScan struct {
	present  map[string]bool
	problems []Problem
}

// decodeStrict runs the structural scan (duplicate keys, unknown fields,
// scalar kinds, nulls, trailing content) and then the typed decode
// (DisallowUnknownFields) into target. It returns the set of JSON paths that
// were present, which semantic validation uses to distinguish "missing"
// from "wrong" and to accept booleans whose zero value is legitimate.
//
// target must be a non-nil pointer to a struct.
func decodeStrict(raw []byte, target any) (map[string]bool, []Problem) {
	t := reflect.TypeOf(target)
	if t == nil || t.Kind() != reflect.Pointer {
		panic("config: decodeStrict requires a pointer target")
	}
	s := &structuralScan{present: map[string]bool{}}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := s.value(specFor(t.Elem()), dec, ""); err != nil {
		return s.present, append(s.problems, Problem{Message: "invalid JSON: " + err.Error()})
	}
	if tok, err := dec.Token(); err == nil {
		s.problems = append(s.problems, Problem{
			Message: fmt.Sprintf("unexpected trailing content after the document: %v", tok),
		})
	} else if !errors.Is(err, io.EOF) {
		s.problems = append(s.problems, Problem{Message: "invalid JSON: " + err.Error()})
	}
	if len(s.problems) > 0 {
		return s.present, s.problems
	}

	// The scan proved the document matches the field set and scalar kinds,
	// so this pass only materializes typed values (identity formats, enum
	// members kept as plain strings, number ranges).
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		s.problems = append(s.problems, typedProblem(err))
	}
	return s.present, s.problems
}

// typedProblem turns an encoding/json error into a path-carrying problem.
func typedProblem(err error) Problem {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		if typeErr.Field != "" {
			return Problem{Path: typeErr.Field, Message: fmt.Sprintf("expected %s, found %s", typeErr.Type, typeErr.Value)}
		}
		return Problem{Message: fmt.Sprintf("expected %s, found %s", typeErr.Type, typeErr.Value)}
	}
	return Problem{Message: "invalid JSON: " + err.Error()}
}

// value consumes exactly one JSON value and checks it against spec.
func (s *structuralScan) value(spec *fieldSpec, dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			return s.object(spec, dec, path)
		case '[':
			return s.array(spec, dec, path)
		default:
			return fmt.Errorf("unexpected %q", v)
		}
	case json.Number:
		switch spec.kind {
		case kindInt:
			if _, err := v.Int64(); err != nil {
				s.add(path, fmt.Sprintf("expected an integer, found %s", v))
			}
		case kindFloat:
			if _, err := v.Float64(); err != nil {
				s.add(path, fmt.Sprintf("expected a number, found %s", v))
			}
		case kindAny:
		default:
			s.add(path, fmt.Sprintf("expected %s, found number %s", spec.kind, v))
		}
	case string:
		if spec.kind != kindString && spec.kind != kindAny {
			s.add(path, fmt.Sprintf("expected %s, found a string", spec.kind))
		}
	case bool:
		if spec.kind != kindBool && spec.kind != kindAny {
			s.add(path, fmt.Sprintf("expected %s, found a boolean", spec.kind))
		}
	case nil:
		if spec.kind != kindAny {
			s.add(path, fmt.Sprintf("expected %s, found null", spec.kind))
		}
	}
	return nil
}

func (s *structuralScan) object(spec *fieldSpec, dec *json.Decoder, path string) error {
	inner := spec
	if spec.kind != kindObject {
		s.add(path, fmt.Sprintf("expected %s, found an object", spec.kind))
		inner = &fieldSpec{kind: kindAny}
	}
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("object key is not a string")
		}
		child := key
		if path != "" {
			child = path + "." + key
		}
		if seen[key] {
			s.add(path, fmt.Sprintf("duplicate key %q", key))
		}
		seen[key] = true

		childSpec := &fieldSpec{kind: kindAny}
		if inner.kind == kindObject {
			switch {
			case inner.dynamic:
				if inner.elem != nil {
					childSpec = inner.elem
				}
			case inner.fields != nil:
				if known, ok := inner.fields[key]; ok {
					childSpec = known
				} else {
					s.add(path, fmt.Sprintf("unknown field %q", key))
				}
			default:
				s.add(path, fmt.Sprintf("unknown field %q", key))
			}
		}
		s.present[child] = true
		if err := s.value(childSpec, dec, child); err != nil {
			return err
		}
	}
	_, err := dec.Token() // closing '}'
	return err
}

func (s *structuralScan) array(spec *fieldSpec, dec *json.Decoder, path string) error {
	if spec.kind == kindObject {
		s.add(path, "expected an object, found an array")
	} else if spec.kind != kindArray && spec.kind != kindAny {
		s.add(path, fmt.Sprintf("expected %s, found an array", spec.kind))
	}
	elem := spec.elem
	if elem == nil {
		elem = &fieldSpec{kind: kindAny}
	}
	for i := 0; dec.More(); i++ {
		if err := s.value(elem, dec, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	_, err := dec.Token() // closing ']'
	return err
}

func (s *structuralScan) add(path, message string) {
	s.problems = append(s.problems, Problem{Path: path, Message: message})
}
