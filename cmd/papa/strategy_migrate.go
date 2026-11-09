package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/pkg/strategymigrate"
)

type strategyManifest struct {
	Version int                         `json:"version"`
	Tables  []strategymigrate.TableSpec `json:"tables"`
}

func readStrategyManifest(path string) ([]strategymigrate.TableSpec, error) {
	if path == "" {
		return nil, nil
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, errors.New("cannot open business migration manifest")
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil || len(data) > 65536 {
		return nil, errors.New("business migration manifest exceeds 64 KiB or is unreadable")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var m strategyManifest
	if d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF || m.Version != 1 {
		return nil, errors.New("invalid business migration manifest (expected version 1)")
	}
	if e = strategymigrate.ValidateTables(m.Tables); e != nil {
		return nil, e
	}
	return m.Tables, nil
}

// Only inspect is implemented: no write flag, import, DROP or application boot.
func runStrategyMigrateCmd(args []string) error {
	if len(args) == 0 || args[0] != "inspect" {
		return errors.New("usage: papa strategy-migrate inspect [-c configs/config.yaml] [--manifest business.json] [--out new-report.json]; only read-only inspection is available")
	}
	f := flag.NewFlagSet("strategy-migrate inspect", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	cfgPath := f.String("c", "", "configuration path")
	f.StringVar(cfgPath, "config", "", "configuration path")
	manifest := f.String("manifest", "", "business-owned table inventory manifest")
	out := f.String("out", "", "new JSON report file; default stdout")
	if e := f.Parse(args[1:]); e != nil || f.NArg() != 0 {
		return errors.New("invalid strategy inspection arguments")
	}
	extra, e := readStrategyManifest(*manifest)
	if e != nil {
		return e
	}
	cfg, ok, e := loadOptionalConfig(*cfgPath)
	if e != nil || !ok {
		return errors.New("strategy inspection configuration unavailable")
	}
	dsn, e := driver.ParseDSN(cfg.DB.DSN)
	if e != nil || dsn.DBName == "" {
		return errors.New("strategy inspection requires a named MySQL database")
	}
	db, e := sql.Open("mysql", cfg.DB.DSN)
	if e != nil {
		return errors.New("strategy inspection connection unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, e := strategymigrate.Inspect(ctx, db, extra)
	if e != nil {
		return e
	}
	data, e := json.MarshalIndent(report, "", "  ")
	if e != nil {
		return e
	}
	data = append(data, '\n')
	if *out == "" {
		_, e = os.Stdout.Write(data)
		return e
	}
	if e = writeStrategyInspection(*out, data); e != nil {
		return e
	}
	fmt.Println("只读盘点完成；未迁移、未删表。行数不能替代内容、引用与文件核验。")
	return nil
}

func writeStrategyInspection(path string, data []byte) error {
	file, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return errors.New("inspection output must be a new writable file")
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("inspection output incomplete; no migration was performed")
	}
	return nil
}
