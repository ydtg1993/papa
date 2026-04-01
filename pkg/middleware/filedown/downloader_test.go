package filedown

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	_ "path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDownload_Single(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello world"))
	}))
	defer server.Close()

	cfg := DefaultConfig()

	d := NewDownloader(cfg)

	res := d.Download(context.Background(), server.URL, "a.txt")

	require.NoError(t, res.Error)

	data, err := os.ReadFile(res.OutputFile)
	require.NoError(t, err)

	assert.Equal(t, "hello world", string(data))
}

func TestDownload_MultiPart(t *testing.T) {
	content := strings.Repeat("A", 1024*10) // 10KB

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		rangeHeader := r.Header.Get("Range")

		if rangeHeader != "" {
			var start, end int
			_, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
			require.NoError(t, err)

			if end >= len(content) {
				end = len(content) - 1
			}

			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(content[start : end+1]))
			return
		}

		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Write([]byte(content))
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.NumWorkers = 4
	cfg.ChunkSize = 1024

	d := NewDownloader(cfg)

	res := d.Download(context.Background(), server.URL, "b.txt")

	require.NoError(t, res.Error)

	data, _ := os.ReadFile(res.OutputFile)
	assert.Equal(t, content, string(data))
}

func TestDownload_Fallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 不支持 Range
		w.Write([]byte("fallback test"))
	}))
	defer server.Close()

	cfg := DefaultConfig()

	d := NewDownloader(cfg)

	res := d.Download(context.Background(), server.URL, "c.txt")

	require.NoError(t, res.Error)

	data, _ := os.ReadFile(res.OutputFile)
	assert.Equal(t, "fallback test", string(data))
}

func TestDownload_Progress(t *testing.T) {
	content := strings.Repeat("B", 1024*5)

	var progressCalled int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5120")
		w.Write([]byte(content))
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OnProgress = func(done, total int64) {
		atomic.AddInt32(&progressCalled, 1)
	}

	d := NewDownloader(cfg)

	res := d.Download(context.Background(), server.URL, "d.txt")

	require.NoError(t, res.Error)

	assert.Greater(t, progressCalled, int32(0))
}
