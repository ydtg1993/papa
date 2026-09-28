package models

// DetailContent 动漫详情页落地结构（存入 CrawlerTask.Content 的 JSON）。
type DetailContent struct {
	Cover         string               `json:"cover"`     // 封面本地化地址
	CoverURL      string               `json:"cover_url"` // 封面原始 URL
	Title         string               `json:"title"`
	Author        string               `json:"author"`
	Tags          []string             `json:"tags"`
	SeriesContent `json:"series_info"` // 剧集列表信息
}

// SeriesContent 动漫剧集列表。
type SeriesContent struct {
	Series    map[string]string `json:"series"`   // title -> url
	Downloads map[string]bool   `json:"download"` // url -> is downloaded
}

// VideoContent 动漫视频资源（m3u8）。
type VideoContent struct {
	Dir    string `json:"dir"`
	Source string `json:"source"` // m3u8 地址
}
