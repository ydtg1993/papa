package fetchruntime

// CloneDownloads detaches registration values from caller-owned overlays and
// their scalar pointers. Nil keeps its distinct "no override" meaning.
// Both site and stage registration use the same ownership boundary.
func CloneDownloads(v SiteDownloads) SiteDownloads {
	return SiteDownloads{M3U8: v.M3U8.Clone(), FileDown: v.FileDown.Clone()}
}
