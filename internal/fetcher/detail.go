package fetcher

import (
	"context"
	"github.com/ydtg1993/papa/internal/crawler"
)

type FetchDetail struct {
}

func (f *FetchDetail) GetStage() string {
	return "detail"
}

func (f *FetchDetail) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
	//TODO implement me
	panic("implement me")
}
