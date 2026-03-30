package main

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

func main() {
	m3u8Chan := make(chan string)
	var wg sync.WaitGroup

	// 创建浏览器 allocator（显示界面）
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(
		context.Background(),
		chromedp.Flag("headless", false), // 显示浏览器窗口
		chromedp.Flag("window-size", "1280,720"),
		chromedp.Flag("disable-gpu", false),
		chromedp.Flag("no-sandbox", false),
	)
	defer cancelAlloc()

	// 创建 browser context
	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	defer cancelCtx()

	// 监听网络请求
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			if isM3U8(ev.Request.URL) {
				fmt.Println("捕获到 m3u8:", ev.Request.URL)

				// 安全提取 Referer 和 Cookie，防止 nil map 或类型错误
				referer := ""
				if ev.Request.Headers != nil {
					if r, ok := ev.Request.Headers["Referer"]; ok {
						if s, ok := r.(string); ok {
							referer = s
						}
					}
				}
				cookie := ""
				if ev.Request.Headers != nil {
					if c, ok := ev.Request.Headers["Cookie"]; ok {
						if s, ok := c.(string); ok {
							cookie = s
						}
					}
				}
				if referer == "" {
					referer = "https://hanime.tv/"
				}

				wg.Add(1)
				go func(m3u8URL, referer, cookie string) {
					defer wg.Done()
					fmt.Printf("开始下载: %s\n", m3u8URL)
					if err := downloadM3U8(m3u8URL, "", referer, cookie); err != nil {
						fmt.Printf("下载失败: %v\n", err)
					} else {
						fmt.Printf("下载完成: %s\n", m3u8URL)
					}
				}(ev.Request.URL, referer, cookie)
			}
		}
	})

	// 1. 先导航到主页面
	if err := chromedp.Run(ctx,
		chromedp.Navigate("https://hanime.tv/videos/hentai/my-mother-1"),
		chromedp.Sleep(3*time.Second),
		chromedp.WaitVisible(`iframe`, chromedp.ByQuery),
	); err != nil {
		fmt.Printf("导航失败: %v\n", err)
		return
	}

	// 2. 获取 iframe 的 src 属性
	var iframeSrc string
	if err := chromedp.Run(ctx,
		chromedp.AttributeValue(`iframe`, "src", &iframeSrc, nil),
	); err != nil {
		fmt.Printf("获取 iframe src 失败: %v\n", err)
		return
	}
	if iframeSrc == "" {
		fmt.Println("iframe src 为空")
		return
	}

	// 补全为完整 URL
	fullIframeURL := iframeSrc
	if !strings.HasPrefix(iframeSrc, "http") {
		fullIframeURL = "https://hanime.tv" + iframeSrc
	}
	fmt.Printf("导航到播放器页面: %s\n", fullIframeURL)

	// 3. 导航到播放器页面，并尝试触发播放
	if err := chromedp.Run(ctx,
		chromedp.Navigate(fullIframeURL),
		chromedp.Sleep(5*time.Second),
		// 尝试自动播放
		chromedp.Evaluate(`(() => {
			const video = document.querySelector('video');
			if (video) {
				video.play();
				return true;
			}
			const playBtn = document.querySelector('.vjs-big-play-button, .play-button, .play, [aria-label="Play"], .play-tn, button');
			if (playBtn) {
				playBtn.click();
				return true;
			}
			return false;
		})()`, nil),
		chromedp.Sleep(30*time.Second), // 等待 m3u8 请求
	); err != nil {
		fmt.Printf("播放器页面操作失败: %v\n", err)
	}

	// 等待所有下载任务完成
	wg.Wait()
	close(m3u8Chan)
	fmt.Println("所有任务完成，程序退出。")
}

func isM3U8(s string) bool {
	return len(s) > 5 && s[len(s)-5:] == ".m3u8"
}

// downloadM3U8 下载并解密 HLS 流
func downloadM3U8(m3u8URL, outputFile, referer, cookie string) error {
	// 1. 获取 m3u8 内容（带重试）
	var playlist string
	var baseURL string
	var err error
	for i := 0; i < 3; i++ {
		playlist, baseURL, err = fetchPlaylistWithRetry(m3u8URL, referer, cookie)
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("获取 m3u8 失败: %v", err)
	}

	// 2. 解析 m3u8
	keyURL, segments, err := parsePlaylist(playlist, baseURL)
	if err != nil {
		return err
	}

	// 3. 下载密钥（带重试）
	var key []byte
	for i := 0; i < 3; i++ {
		key, err = downloadKeyWithRetry(keyURL, referer, cookie)
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("下载密钥失败: %v", err)
	}

	// 4. 确定输出文件名
	if outputFile == "" {
		u, err := neturl.Parse(m3u8URL)
		if err != nil {
			outputFile = fmt.Sprintf("video_%d.ts", time.Now().Unix())
		} else {
			base := filepath.Base(u.Path)
			base = strings.TrimSuffix(base, ".m3u8")
			if base == "" {
				base = fmt.Sprintf("video_%d", time.Now().Unix())
			}
			outputFile = base + ".ts"
		}
	}

	// 5. 打开输出文件（追加模式，支持断点续传）
	outFile, err := os.OpenFile(outputFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer outFile.Close()

	// 6. 依次下载每个片段（带重试和简单断点续传）
	currentSize, _ := outFile.Seek(0, io.SeekEnd)
	startIdx := 0
	if currentSize > 0 {
		// 粗略估算每个片段大小（约 1MB），仅用于断点续传示例
		estimatedSegSize := 1024 * 1024
		startIdx = int(currentSize) / estimatedSegSize
		if startIdx >= len(segments) {
			startIdx = len(segments)
		}
		fmt.Printf("检测到已有文件 %s，从片段 %d 开始续传\n", outputFile, startIdx)
	}

	for i := startIdx; i < len(segments); i++ {
		fmt.Printf("下载片段 %d/%d: %s\n", i+1, len(segments), segments[i])

		// 计算 IV：片段序号（大端序）
		iv := make([]byte, 16)
		binary.BigEndian.PutUint64(iv[8:], uint64(i))

		var data []byte
		for retry := 0; retry < 3; retry++ {
			data, err = downloadAndDecryptSegment(segments[i], key, iv, referer, cookie)
			if err == nil {
				break
			}
			fmt.Printf("片段 %d 下载失败，重试 %d/3: %v\n", i+1, retry+1, err)
			time.Sleep(2 * time.Second)
		}
		if err != nil {
			return fmt.Errorf("片段 %d 下载失败: %v", i+1, err)
		}

		if _, err := outFile.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// fetchPlaylistWithRetry 带重试的播放列表获取
func fetchPlaylistWithRetry(urlStr, referer, cookie string) (string, string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	// 计算 baseURL
	u, err := neturl.Parse(urlStr)
	if err != nil {
		return "", "", err
	}
	baseURL := u.Scheme + "://" + u.Host + path.Dir(u.Path)
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	return string(data), baseURL, nil
}

// downloadKeyWithRetry 带重试的密钥下载
func downloadKeyWithRetry(keyURL, referer, cookie string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", keyURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	key, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(key) != 16 {
		return nil, fmt.Errorf("密钥长度应为16字节，实际%d", len(key))
	}
	return key, nil
}

// downloadAndDecryptSegment 下载并解密单个片段
func downloadAndDecryptSegment(segURL string, key, iv []byte, referer, cookie string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", segURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	ciphertext, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return decryptAES128CBC(ciphertext, key, iv)
}

// parsePlaylist 解析 m3u8 文件，返回密钥 URL 和片段 URL 列表
func parsePlaylist(playlist, baseURL string) (keyURL string, segments []string, err error) {
	scanner := bufio.NewScanner(strings.NewReader(playlist))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-KEY:") {
			// 提取 URI="..."
			start := strings.Index(line, "URI=\"")
			if start == -1 {
				return "", nil, fmt.Errorf("未找到 URI")
			}
			start += 5
			end := strings.Index(line[start:], "\"")
			if end == -1 {
				return "", nil, fmt.Errorf("URI 格式错误")
			}
			uri := line[start : start+end]
			keyURL = resolveURL(uri, baseURL)
		} else if strings.HasPrefix(line, "#EXTINF:") {
			// 下一行是片段 URL，跳过
			continue
		} else if !strings.HasPrefix(line, "#") {
			segURL := resolveURL(line, baseURL)
			segments = append(segments, segURL)
		}
	}
	if keyURL == "" {
		return "", nil, fmt.Errorf("未找到密钥 URL")
	}
	if len(segments) == 0 {
		return "", nil, fmt.Errorf("未找到任何片段")
	}
	return keyURL, segments, nil
}

// resolveURL 将相对路径转换为绝对 URL
func resolveURL(raw, base string) string {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	if base == "" {
		return raw
	}
	if strings.HasPrefix(raw, "/") {
		u, err := neturl.Parse(base)
		if err == nil {
			u.Path = raw
			return u.String()
		}
	}
	return base + raw
}

// decryptAES128CBC 解密 AES-128-CBC 数据，并去除 PKCS#7 填充
func decryptAES128CBC(ciphertext, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("密文长度不是块大小的整数倍")
	}
	mode := cipher.NewCBCDecrypter(block, iv)
	plaintext := make([]byte, len(ciphertext))
	mode.CryptBlocks(plaintext, ciphertext)

	// 去除 PKCS#7 填充
	if len(plaintext) > 0 {
		paddingLen := int(plaintext[len(plaintext)-1])
		if paddingLen <= aes.BlockSize && paddingLen > 0 {
			plaintext = plaintext[:len(plaintext)-paddingLen]
		}
	}
	return plaintext, nil
}
