package frameworkdata

import (
	"context"
	"errors"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/admin/gormsource"
	"gorm.io/gorm"
)

type source struct {
	db    *gorm.DB
	inner *gormsource.Source
}

func (s *source) List(ctx context.Context, q oao.Query) ([]map[string]any, int64, error) {
	if s.db == nil {
		return nil, 0, errors.New("framework data unavailable")
	}
	// The oao parser bounds HTTP pages too; enforce the same bounds for direct use.
	if q.Page < 1 {
		q.Page = 1
	}
	if q.Size < 1 {
		q.Size = 20
	}
	if q.Size > 200 {
		q.Size = 200
	}
	if q.Page > 1000000 {
		return nil, 0, errors.New("framework data page out of range")
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	rows, total, err := s.inner.List(ctx, q)
	if err != nil {
		return nil, 0, errors.New("framework data query unavailable")
	}
	for _, row := range rows {
		for k, v := range row {
			row[k] = sanitizeValue(k, v, 0)
		}
	}
	return rows, total, nil
}
