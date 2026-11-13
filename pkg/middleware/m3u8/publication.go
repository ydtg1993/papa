package m3u8

import (
	"context"
	"github.com/ydtg1993/papa/v4/pkg/middleware"
	"io"
	"os"
	"path/filepath"
)

// publishFile owns staging and atomic replacement; failure leaves the old output.
func publishFile(ctx context.Context, path string, limit int64, write func(string) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".papa-publish-*"+filepath.Ext(path))
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err = f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	defer os.Remove(tmp)
	if err = os.Chmod(tmp, 0644); err != nil {
		return err
	}
	if info, e := os.Stat(path); e == nil {
		if e = os.Chmod(tmp, info.Mode().Perm()); e != nil {
			return e
		}
	}
	if err = write(tmp); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	if info.Size() > limit {
		return middleware.ResourceLimit("published output bytes", limit)
	}
	return os.Rename(tmp, path)
}
func (d *Downloader) concatTSFilesContext(ctx context.Context, init string, files []string, path string) error {
	return publishFile(ctx, path, d.limits.OutputBytes, func(tmp string) error {
		out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return err
		}
		defer out.Close()
		var written int64
		buf := make([]byte, 32<<10)
		for _, p := range append([]string{init}, files...) {
			if p == "" {
				continue
			}
			err := func() error {
				in, e := os.Open(p)
				if e != nil {
					return e
				}
				defer in.Close()
				for {
					if e := ctx.Err(); e != nil {
						return e
					}
					n, e := in.Read(buf)
					if n > 0 {
						if int64(n) > d.limits.OutputBytes-written {
							return middleware.ResourceLimit("assembled output bytes", d.limits.OutputBytes)
						}
						if _, e := out.Write(buf[:n]); e != nil {
							return e
						}
						written += int64(n)
					}
					if e == io.EOF {
						return nil
					}
					if e != nil {
						return e
					}
				}
			}()
			if err != nil {
				return err
			}
		}
		return out.Close()
	})
}

// boundedProcessLog drains subprocess diagnostics without retaining unbounded output.
type boundedProcessLog struct{ data []byte }

func (b *boundedProcessLog) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (64 << 10) - len(b.data)
	if remaining > 0 {
		b.data = append(b.data, p[:min(len(p), remaining)]...)
	}
	return n, nil
}
