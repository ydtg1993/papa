package notify

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/egress"
	"github.com/ydtg1993/papa/v3/pkg/redact"
)

// Webhook 通过 HTTP POST 将告警事件以 JSON 发送到指定 URL（可接钉钉/webhook 网关）。
type Webhook struct {
	Network egress.Config
	URL     string
	Client  *http.Client
}

// NewWebhook 创建 webhook 通知器。
func NewWebhook(url string) *Webhook {
	return &Webhook{URL: url, Client: &http.Client{Timeout: 10 * time.Second}}
}

// Notify 发送告警事件到 webhook URL。
func (w *Webhook) Notify(ctx context.Context, event core.AlertEvent) error {
	if w.URL == "" {
		return fmt.Errorf("webhook url is empty")
	}
	body, err := redact.JSON(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	guarded := egress.Client(client, w.Network)
	defer guarded.CloseIdleConnections()
	resp, err := guarded.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("webhook %s returned status %d", redact.URL(w.URL), resp.StatusCode)
	}
	return nil
}
