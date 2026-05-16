// Package audit — aliyun.go calls the Aliyun Green (content-safety) service
// to review post text and images synchronously.
//
// ── API surface used ─────────────────────────────────────────────────────────
//
//   Text:  POST /green/text/scan   scenes=["antispam"]
//   Image: POST /green/image/scan  scenes=["porn","terrorism","ad","qrcode"]
//
// Both endpoints return a suggestion per task: "pass" | "review" | "block".
// We map the worst suggestion across all tasks to a single AuditStatus:
//
//   "block"  → AuditStatusMachineDeny (4)
//   "review" → AuditStatusSuspect     (2)
//   "pass"   → AuditStatusMachinePass (1)
//
// ── Error handling ──────────────────────────────────────────────────────────
//
// If the Green API is unreachable or returns an unexpected HTTP code,
// Review returns an error. AuditConsumer treats this as a degradation:
// it logs the failure but does NOT update audit_status, leaving the post
// in the pending (is_visible=0) state for a future retry or human review.
//
// ── Thread safety ───────────────────────────────────────────────────────────
//
// alibaba-cloud-sdk-go clients are safe for concurrent use. AuditConsumer
// reuses the single AliyunAuditor instance across all goroutines.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"cooking-platform/internal/model"
	"cooking-platform/pkg/config"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/green"
	"github.com/google/uuid"
)

// AliyunAuditor wraps the Aliyun Green client.
type AliyunAuditor struct {
	client *green.Client
}

// NewAliyunAuditor constructs an AliyunAuditor.
// The Green client is created once at startup and reused for all requests.
func NewAliyunAuditor(cfg config.AuditConfig) (*AliyunAuditor, error) {
	client, err := green.NewClientWithAccessKey(cfg.Region, cfg.AccessKeyID, cfg.AccessKeySecret)
	if err != nil {
		return nil, fmt.Errorf("new green client: %w", err)
	}
	return &AliyunAuditor{client: client}, nil
}

// Review calls text scan and (if images are present) image scan, then
// returns the worst verdict across both scans.
func (a *AliyunAuditor) Review(ctx context.Context, req ReviewRequest) (ReviewResult, error) {
	// Combine title + content as a single text task.
	text := strings.TrimSpace(req.Title + "\n" + req.Content)

	textResult, textRaw, err := a.reviewText(text)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("green text scan: %w", err)
	}

	finalStatus := textResult
	allRaw := map[string]interface{}{"text": json.RawMessage(textRaw)}

	if len(req.ImageURLs) > 0 {
		imgResult, imgRaw, err := a.reviewImages(req.ImageURLs)
		if err != nil {
			return ReviewResult{}, fmt.Errorf("green image scan: %w", err)
		}
		finalStatus = worstStatus(finalStatus, imgResult)
		allRaw["image"] = json.RawMessage(imgRaw)
	}

	rawBytes, _ := json.Marshal(allRaw)
	remark := remarkFor(finalStatus)
	return ReviewResult{
		Status: finalStatus,
		Remark: remark,
		Raw:    string(rawBytes),
	}, nil
}

// ── text scan ────────────────────────────────────────────────────────────────

type textScanBody struct {
	Scenes []string       `json:"scenes"`
	Tasks  []textScanTask `json:"tasks"`
}

type textScanTask struct {
	DataID  string `json:"dataId"`
	Content string `json:"content"`
}

type textScanResponse struct {
	Code int                  `json:"code"`
	Data []textScanDataItem   `json:"data"`
}

type textScanDataItem struct {
	Code    int                    `json:"code"`
	Results []textScanResultDetail `json:"results"`
}

type textScanResultDetail struct {
	Scene      string `json:"scene"`
	Suggestion string `json:"suggestion"` // pass | review | block
}

func (a *AliyunAuditor) reviewText(text string) (uint8, string, error) {
	body := textScanBody{
		Scenes: []string{"antispam"},
		Tasks:  []textScanTask{{DataID: uuid.NewString(), Content: text}},
	}
	bodyBytes, _ := json.Marshal(body)

	req := green.CreateTextScanRequest()
	req.SetContent(bodyBytes)

	resp, err := a.client.TextScan(req)
	if err != nil {
		return 0, "", fmt.Errorf("TextScan RPC: %w", err)
	}

	raw := resp.GetHttpContentString()
	var result textScanResponse
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return 0, raw, fmt.Errorf("parse TextScan response: %w", err)
	}
	if result.Code != 200 {
		return 0, raw, fmt.Errorf("TextScan API error code: %d", result.Code)
	}

	status := model.AuditStatusMachinePass
	for _, item := range result.Data {
		for _, r := range item.Results {
			status = worstStatus(status, suggestionToStatus(r.Suggestion))
		}
	}
	return status, raw, nil
}

// ── image scan ───────────────────────────────────────────────────────────────

type imageScanBody struct {
	Scenes []string        `json:"scenes"`
	Tasks  []imageScanTask `json:"tasks"`
}

type imageScanTask struct {
	DataID string `json:"dataId"`
	URL    string `json:"url"`
}

type imageScanResponse struct {
	Code int                   `json:"code"`
	Data []imageScanDataItem   `json:"data"`
}

type imageScanDataItem struct {
	Code    int                     `json:"code"`
	Results []imageScanResultDetail `json:"results"`
}

type imageScanResultDetail struct {
	Scene      string `json:"scene"`
	Suggestion string `json:"suggestion"` // pass | review | block
}

func (a *AliyunAuditor) reviewImages(urls []string) (uint8, string, error) {
	tasks := make([]imageScanTask, 0, len(urls))
	for _, u := range urls {
		tasks = append(tasks, imageScanTask{DataID: uuid.NewString(), URL: u})
	}
	body := imageScanBody{
		Scenes: []string{"porn", "terrorism", "ad", "qrcode"},
		Tasks:  tasks,
	}
	bodyBytes, _ := json.Marshal(body)

	req := green.CreateImageSyncScanRequest()
	req.SetContent(bodyBytes)

	resp, err := a.client.ImageSyncScan(req)
	if err != nil {
		return 0, "", fmt.Errorf("ImageScan RPC: %w", err)
	}

	raw := resp.GetHttpContentString()
	var result imageScanResponse
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return 0, raw, fmt.Errorf("parse ImageScan response: %w", err)
	}
	if result.Code != 200 {
		return 0, raw, fmt.Errorf("ImageScan API error code: %d", result.Code)
	}

	status := model.AuditStatusMachinePass
	for _, item := range result.Data {
		for _, r := range item.Results {
			status = worstStatus(status, suggestionToStatus(r.Suggestion))
		}
	}
	return status, raw, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// suggestionToStatus maps Aliyun's suggestion string to our model enum.
func suggestionToStatus(s string) uint8 {
	switch s {
	case "block":
		return model.AuditStatusMachineDeny
	case "review":
		return model.AuditStatusSuspect
	default:
		return model.AuditStatusMachinePass
	}
}

func remarkFor(status uint8) string {
	switch status {
	case model.AuditStatusMachineDeny:
		return "machine review: content violates policy"
	case model.AuditStatusSuspect:
		return "machine review: content requires human review"
	default:
		return "machine review: pass"
	}
}
