package outis

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

var (
	contextType = reflect.TypeFor[context.Context]()
	errorType   = reflect.TypeFor[error]()
)

type idempotencyKeySetter interface{ SetIdempotencyKey(string) }

// callMethod resolves method on reg's client and calls it with args.
func callMethod(ctx context.Context, reg *registered, method string, args []json.RawMessage, key string) (ref string, err error) {
	fn, err := resolveMethod(reflect.ValueOf(reg.target), method)
	if err != nil {
		return "", &IntentError{Reason: ReasonNotRegistered, Err: err}
	}
	in, err := decodeArgs(ctx, fn.Type(), args)
	if err != nil {
		return "", &IntentError{Reason: ReasonBadArgs, Err: err}
	}
	if reg.injectKey {
		for _, a := range in {
			if a.Kind() == reflect.Pointer && a.IsNil() {
				continue
			}
			if s, ok := a.Interface().(idempotencyKeySetter); ok {
				s.SetIdempotencyKey(key)
			}
		}
	}

	defer func() {
		if p := recover(); p != nil {
			ref, err = "", &IntentError{Reason: ReasonExecutionError, Err: fmt.Errorf("panic: %v", p)}
		}
	}()
	out := fn.Call(in)
	var result reflect.Value
	for i, v := range out {
		if i == len(out)-1 && fn.Type().Out(i) == errorType {
			if !v.IsNil() {
				return "", &IntentError{Reason: ReasonExecutionError, Err: v.Interface().(error)}
			}
			continue
		}
		if !result.IsValid() {
			result = v
		}
	}
	return referenceOf(result), nil
}

// resolveMethod walks a dotted path: exported fields and no-argument accessor
// methods, then a method.
func resolveMethod(v reflect.Value, dotted string) (reflect.Value, error) {
	segs := strings.Split(dotted, ".")
	for i, seg := range segs {
		if seg == "" {
			return reflect.Value{}, fmt.Errorf("method %q has an empty segment", dotted)
		}
		last := i == len(segs)-1
		m, err := methodNamed(v, seg)
		if err != nil {
			return reflect.Value{}, err
		}
		if m.IsValid() {
			if last {
				return m, nil
			}
			if t := m.Type(); t.NumIn() != 0 || t.NumOut() != 1 {
				return reflect.Value{}, fmt.Errorf("%s in %q isn't an accessor", seg, dotted)
			}
			v = m.Call(nil)[0]
			continue
		}
		if last {
			return reflect.Value{}, fmt.Errorf("no method %s in %q", seg, dotted)
		}
		f, err := fieldNamed(v, seg)
		if err != nil {
			return reflect.Value{}, err
		}
		if !f.IsValid() {
			return reflect.Value{}, fmt.Errorf("no field or method %s in %q", seg, dotted)
		}
		v = f
	}
	return reflect.Value{}, fmt.Errorf("method %q is empty", dotted)
}

// foldName matches an intent segment to a Go name: exactly, or ignoring case
// and underscores, so other SDKs' "transfers.create" reaches Transfers.Create.
func foldName(seg, name string) bool {
	return strings.EqualFold(strings.ReplaceAll(seg, "_", ""), name)
}

func methodNamed(v reflect.Value, seg string) (reflect.Value, error) {
	for v.Kind() == reflect.Interface && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() != reflect.Pointer && v.CanAddr() {
		v = v.Addr()
	}
	if !v.IsValid() {
		return reflect.Value{}, nil
	}
	if m := v.MethodByName(seg); m.IsValid() {
		return m, nil
	}
	var found reflect.Value
	for i := 0; i < v.NumMethod(); i++ {
		if foldName(seg, v.Type().Method(i).Name) {
			if found.IsValid() {
				return reflect.Value{}, fmt.Errorf("%s matches more than one method", seg)
			}
			found = v.Method(i)
		}
	}
	return found, nil
}

func fieldNamed(v reflect.Value, seg string) (reflect.Value, error) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return reflect.Value{}, fmt.Errorf("%s is on a nil value", seg)
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, nil
	}
	t := v.Type()
	if f, ok := t.FieldByName(seg); ok && f.IsExported() {
		return v.FieldByIndex(f.Index), nil
	}
	var found reflect.Value
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); f.IsExported() && foldName(seg, f.Name) {
			if found.IsValid() {
				return reflect.Value{}, fmt.Errorf("%s matches more than one field", seg)
			}
			found = v.Field(i)
		}
	}
	return found, nil
}

// decodeArgs builds a call's arguments: ctx for a leading context.Context,
// then each JSON arg decoded into its parameter's type.
func decodeArgs(ctx context.Context, t reflect.Type, args []json.RawMessage) ([]reflect.Value, error) {
	var in []reflect.Value
	first := 0
	if t.NumIn() > 0 && t.In(0) == contextType {
		in = append(in, reflect.ValueOf(ctx))
		first = 1
	}
	fixed := t.NumIn() - first
	if t.IsVariadic() {
		fixed--
		if len(args) < fixed {
			return nil, fmt.Errorf("want at least %d args, got %d", fixed, len(args))
		}
	} else if len(args) != fixed {
		return nil, fmt.Errorf("want %d args, got %d", fixed, len(args))
	}
	for i, raw := range args {
		var pt reflect.Type
		if i < fixed {
			pt = t.In(first + i)
		} else {
			pt = t.In(t.NumIn() - 1).Elem()
		}
		p := reflect.New(pt)
		if err := json.Unmarshal(raw, p.Interface()); err != nil {
			return nil, fmt.Errorf("arg %d into %s: %w", i, pt, err)
		}
		in = append(in, p.Elem())
	}
	return in, nil
}

// referenceOf is a result's ID field, its GetID(), the result itself when
// it's a string, or "".
func referenceOf(v reflect.Value) string {
	orig := v
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return ""
	}
	if v.Kind() == reflect.Struct {
		if f, ok := v.Type().FieldByName("ID"); ok && f.IsExported() {
			id := v.FieldByIndex(f.Index)
			if id.Kind() == reflect.String {
				return id.String()
			}
			return fmt.Sprint(id.Interface())
		}
	}
	if g, ok := orig.Interface().(interface{ GetID() string }); ok {
		return g.GetID()
	}
	if v.Kind() == reflect.String {
		return v.String()
	}
	return ""
}
