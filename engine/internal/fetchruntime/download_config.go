package fetchruntime

import (
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
)

// CloneDownloads detaches registration values from caller-owned overlays and
// their scalar pointers. Nil keeps its distinct "no override" meaning.
// Both site and stage registration use the same ownership boundary.
func CloneDownloads(v SiteDownloads) SiteDownloads {
	var out SiteDownloads
	if o := v.M3U8; o != nil {
		out.M3U8 = &m3u8.Overlay{
			OutputDir:      cloneValue(o.OutputDir),
			ResumeStateDir: cloneValue(o.ResumeStateDir),
			Output:         cloneValue(o.Output),
			MergeExt:       cloneValue(o.MergeExt),
			KeepSegments:   cloneValue(o.KeepSegments),
			MaxConcurrent:  cloneValue(o.MaxConcurrent),
			SegmentTimeout: cloneValue(o.SegmentTimeout),
			MaxRetries:     cloneValue(o.MaxRetries),
			RetryInterval:  cloneValue(o.RetryInterval),
			EnableResume:   cloneValue(o.EnableResume),
			RateKB:         cloneValue(o.RateKB),
			SaveBatchSize:  cloneValue(o.SaveBatchSize),
			FfmpegPath:     cloneValue(o.FfmpegPath),
			QueueSize:      cloneValue(o.QueueSize),
		}
	}
	if o := v.FileDown; o != nil {
		out.FileDown = &filedown.Overlay{
			OutputDir:              cloneValue(o.OutputDir),
			ResumeStateDir:         cloneValue(o.ResumeStateDir),
			MaxRetries:             cloneValue(o.MaxRetries),
			RetryInterval:          cloneValue(o.RetryInterval),
			Timeout:                cloneValue(o.Timeout),
			MaxConcurrent:          cloneValue(o.MaxConcurrent),
			ChunkSize:              cloneValue(o.ChunkSize),
			EnableResume:           cloneValue(o.EnableResume),
			SaveBatchSize:          cloneValue(o.SaveBatchSize),
			KeepSegmentsAfterMerge: cloneValue(o.KeepSegmentsAfterMerge),
			QueueSize:              cloneValue(o.QueueSize),
		}
	}
	return out
}

func cloneValue[T ~string | ~int | ~int64 | ~bool](v *T) *T {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}
