package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// traceData preflights ordinary values before encoding, preserving encoding/json
// tags, byte slices, raw JSON and custom marshalers. Custom marshalers are caller
// code: their transient allocations cannot be bounded, but their output is capped
// and panics are converted to evidence rather than escaping into the handler.
func traceData(data any) (result string, truncated bool) {
	defer func() {
		if r := recover(); r != nil {
			result = `{"_marshal_error":"serializer panic"}`
			truncated = true
		}
	}()
	budget, nodes := maxTraceDataBytes, 1024
	var fits func(reflect.Value, int) bool
	fits = func(v reflect.Value, depth int) bool {
		nodes--
		budget -= 16
		if nodes < 0 || budget < 0 || depth > 12 {
			return false
		}
		if !v.IsValid() {
			return true
		}
		if v.CanInterface() {
			if raw, ok := v.Interface().(json.RawMessage); ok {
				budget -= len(raw)
				return budget >= 0
			}
			if _, ok := v.Interface().(json.Marshaler); ok {
				return true
			}
		}
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if v.IsNil() {
				return true
			}
			return fits(v.Elem(), depth+1)
		case reflect.String:
			budget -= v.Len()
			return budget >= 0
		case reflect.Array, reflect.Slice:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				budget -= v.Len() * 2
				return budget >= 0
			}
			for i := 0; i < v.Len(); i++ {
				if !fits(v.Index(i), depth+1) {
					return false
				}
			}
			return true
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				if !fits(iter.Key(), depth+1) || !fits(iter.Value(), depth+1) {
					return false
				}
			}
			return true
		case reflect.Struct:
			typ := v.Type()
			for i := 0; i < v.NumField(); i++ {
				nodes--
				if nodes < 0 {
					return false
				}
				field := typ.Field(i)
				if field.PkgPath != "" && !field.Anonymous {
					continue
				}
				budget -= len(field.Name) + len(field.Tag)
				if budget < 0 {
					return false
				}
				if strings.Split(field.Tag.Get("json"), ",")[0] == "-" {
					continue
				}
				if !fits(v.Field(i), depth+1) {
					return false
				}
			}
			return true
		default:
			return true
		}
	}
	if !fits(reflect.ValueOf(data), 0) {
		return `{"_truncated":true}`, true
	}
	b, err := marshalNoHTMLEscape(data)
	if err != nil {
		b, _ = marshalNoHTMLEscape(map[string]string{"_marshal_error": traceClip(fmt.Sprint(err), 1024)})
	}
	if len(b) > maxTraceDataBytes {
		return `{"_truncated":true}`, true
	}
	return string(b), false
}
