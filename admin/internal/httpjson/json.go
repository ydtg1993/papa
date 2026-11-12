// Package httpjson implements the shared bounded-handler JSON wire conventions.
package httpjson

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func Strict(r io.Reader, v any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("请求必须包含一个 JSON 对象")
	}
	return nil
}
func Write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
