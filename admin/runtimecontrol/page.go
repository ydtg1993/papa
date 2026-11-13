package runtimecontrol

import "github.com/ydtg1993/papa/v4/admin/page"

func Page() page.Descriptor {
	return page.Descriptor{Key: "crawl-control", Label: "采集控制", Section: "monitor", Script: Script}
}
