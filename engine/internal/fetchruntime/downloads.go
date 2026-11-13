package fetchruntime

import (
	"fmt"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/middleware"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
	"sync"
)

type SiteDownloads struct {
	M3U8     *m3u8.Overlay
	FileDown *filedown.Overlay
}
type StageDownloads struct {
	Site, Stage string
	SiteDownloads
}

// Downloads owns configuration and resolved instances. Resolve publishes one
// complete snapshot under the same lock used by registration and lookup.
type Downloads struct {
	resourceLimits          middleware.DownloadLimits
	resourceBudget          *middleware.DownloadBudget
	resourcesFrozen         bool
	mu                      sync.RWMutex
	sites                   map[string]SiteDownloads
	m3u8ByStage, m3u8BySite map[string]*m3u8.Downloader
	fileByStage, fileBySite map[string]*filedown.Downloader
}

func (d *Downloads) Register(site string, v SiteDownloads) {
	if site == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sites == nil {
		d.sites = make(map[string]SiteDownloads)
	}
	d.sites[site] = CloneDownloads(v)
}
func (d *Downloads) Resolve(stages []StageDownloads, m *m3u8.Downloader, f *filedown.Downloader) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m3u8ByStage = make(map[string]*m3u8.Downloader)
	d.m3u8BySite = make(map[string]*m3u8.Downloader)
	d.fileByStage = make(map[string]*filedown.Downloader)
	d.fileBySite = make(map[string]*filedown.Downloader)
	for _, stage := range stages {
		ov := d.sites[stage.Site]
		key := core.StageKey(stage.Site, stage.Stage)
		if merged := MergeM3U8(ov.M3U8, stage.M3U8); !merged.IsZero() && m != nil {
			d.m3u8ByStage[key] = m.With(merged)
		}
		if merged := MergeFileDown(ov.FileDown, stage.FileDown); !merged.IsZero() && f != nil {
			d.fileByStage[key] = f.With(merged)
		}
	}
	for site, ov := range d.sites {
		if ov.M3U8 != nil && !ov.M3U8.IsZero() && m != nil {
			d.m3u8BySite[site] = m.With(*ov.M3U8)
		}
		if ov.FileDown != nil && !ov.FileDown.IsZero() && f != nil {
			d.fileBySite[site] = f.With(*ov.FileDown)
		}
	}
}
func (d *Downloads) M3U8Stage(site, stage string, fallback *m3u8.Downloader) *m3u8.Downloader {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if v := d.m3u8ByStage[core.StageKey(site, stage)]; v != nil {
		return v
	}
	return fallback
}
func (d *Downloads) M3U8Site(site string, fallback *m3u8.Downloader) *m3u8.Downloader {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if v := d.m3u8BySite[site]; v != nil {
		return v
	}
	return fallback
}
func (d *Downloads) FileStage(site, stage string, fallback *filedown.Downloader) *filedown.Downloader {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if v := d.fileByStage[core.StageKey(site, stage)]; v != nil {
		return v
	}
	return fallback
}
func (d *Downloads) FileSite(site string, fallback *filedown.Downloader) *filedown.Downloader {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if v := d.fileBySite[site]; v != nil {
		return v
	}
	return fallback
}
func MergeM3U8(site, stage *m3u8.Overlay) m3u8.Overlay {
	var out m3u8.Overlay
	if site != nil {
		out = *site
	}
	if stage != nil {
		out = out.Then(*stage)
	}
	return out
}

// MergeFileDown 同上，文件下载器那份。
func MergeFileDown(site, stage *filedown.Overlay) filedown.Overlay {
	var out filedown.Overlay
	if site != nil {
		out = *site
	}
	if stage != nil {
		out = out.Then(*stage)
	}
	return out
}

// ConfigureResources is composition-only. Resource policy is frozen when bases
// are constructed, before overlays publish any derived downloader.
func (d *Downloads) ConfigureResources(limits middleware.DownloadLimits, budget *middleware.DownloadBudget) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.resourcesFrozen {
		return fmt.Errorf("download resources already initialized")
	}
	d.resourceLimits, d.resourceBudget = limits.Normalized(), budget
	return nil
}

// Resources returns the one shared budget for both base downloader families.
func (d *Downloads) Resources() (middleware.DownloadLimits, *middleware.DownloadBudget) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.resourceBudget == nil {
		d.resourceBudget = middleware.NewDownloadBudget(0, 0)
	}
	d.resourceLimits = d.resourceLimits.Normalized()
	d.resourcesFrozen = true
	return d.resourceLimits, d.resourceBudget
}
