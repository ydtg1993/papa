package m3u8

import (
	"context"
	"github.com/ydtg1993/papa/v4/pkg/middleware"
	"io"
	"os"
)

func (d *Downloader) accountBytes(n int64) error {
	if d.received != nil && d.received.Add(n) > d.limits.OutputBytes {
		return middleware.ResourceLimit("download output bytes (including retries)", d.limits.OutputBytes)
	}
	return nil
}

// copySegment limits receive buffers and applies rate control before each read.
func (d *Downloader) copySegment(ctx context.Context, dst io.Writer, src io.Reader, limit int64, limiter *RateLimiter) error {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		size := int64(len(buf))
		if limiter != nil && limiter.limiter != nil {
			size = min(size, int64(limiter.limiter.Burst()))
		}
		if limit-total+1 < size {
			size = limit - total + 1
		}
		if err := limiter.Wait(ctx, int(size)); err != nil {
			return err
		}
		n, err := src.Read(buf[:size])
		if n > 0 {
			if int64(n) > limit-total {
				return middleware.ResourceLimit("media bytes", limit)
			}
			if e := d.accountBytes(int64(n)); e != nil {
				return e
			}
			if _, e := dst.Write(buf[:n]); e != nil {
				return e
			}
			total += int64(n)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
func (d *Downloader) streamSegment(ctx context.Context, seg *SegmentInfo, limiter *RateLimiter, dest string, opts *DownloadOptions) error {
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(tmp) }()
	rng := ""
	if seg.Range != "" {
		rng = "bytes=" + seg.Range
	}
	target := &rewindWriter{file: f}
	_, _, err = d.requestWithLimit(ctx, seg.URL, rng, opts, d.limits.MediaBytes, target, limiter)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

type rewindWriter struct{ file *os.File }

func (w *rewindWriter) Write(p []byte) (int, error) { return w.file.Write(p) }
func (w *rewindWriter) reset() error {
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	_, err := w.file.Seek(0, 0)
	return err
}
