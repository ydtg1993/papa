package models

import (
	"context"
	"fmt"
	"gorm.io/gorm/schema"
	"reflect"
	"time"
)

func init() { schema.RegisterSerializer("papa_utc", utcTimeSerializer{}) }

// DATETIME carries no timezone. These new fields always store UTC wall time;
// sending a string prevents the MySQL driver's Loc from converting it again.
type utcTimeSerializer struct{}

func utcWallTime(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

func (utcTimeSerializer) Value(_ context.Context, _ *schema.Field, _ reflect.Value, value interface{}) (interface{}, error) {
	if value == nil {
		return nil, nil
	}
	switch t := value.(type) {
	case *time.Time:
		if t == nil {
			return nil, nil
		}
		return t.UTC().Format("2006-01-02 15:04:05.000"), nil
	case time.Time:
		return t.UTC().Format("2006-01-02 15:04:05.000"), nil
	default:
		return nil, fmt.Errorf("UTC timestamp: unsupported value %T", value)
	}
}

func (utcTimeSerializer) Scan(ctx context.Context, field *schema.Field, dst reflect.Value, value interface{}) error {
	var at *time.Time
	if value != nil {
		var t time.Time
		var err error
		switch v := value.(type) {
		case time.Time:
			t = utcWallTime(v)
		case string:
			t, err = time.ParseInLocation("2006-01-02 15:04:05.999999", v, time.UTC)
		case []byte:
			t, err = time.ParseInLocation("2006-01-02 15:04:05.999999", string(v), time.UTC)
		default:
			return fmt.Errorf("UTC timestamp: unsupported database value %T", value)
		}
		if err != nil {
			return err
		}
		at = &t
	}
	return field.Set(ctx, dst, at)
}
