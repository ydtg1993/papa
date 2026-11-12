package runtimecontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/config"
	"io"
)

type Document struct {
	SchemaVersion int                  `json:"schema_version"`
	Overrides     config.RuntimeConfig `json:"overrides"`
}

func Decode(raw []byte) (Document, error) {
	doc := Document{SchemaVersion: 1}
	if len(raw) == 0 {
		return doc, nil
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return doc, errors.New("运行策略必须是 JSON 对象")
	}
	if err := strictJSON(bytes.NewReader(raw), &doc); err != nil {
		return doc, err
	}
	if doc.SchemaVersion != 1 {
		return doc, fmt.Errorf("不支持的运行策略 schema_version %d", doc.SchemaVersion)
	}
	return doc, nil
}
func strictJSON(r io.Reader, v any) error {
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
