package fetcher

import (
	"context"
	"github.com/ydtg1993/papa/internal/crawler"
)

type FetchVideo struct {
}

func (f *FetchVideo) GetStage() string {
	return "video"
}

func (f *FetchVideo) FetchHandler(ctx context.Context, task *crawler.Task, engine *crawler.Engine) error {
	//TODO implement me
	panic("implement me")
}
