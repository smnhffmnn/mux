package vault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Notifier sends approval requests to the user.
// Implementations: DiscordWebhookNotifier (now), APNsNotifier (future iOS).
type Notifier interface {
	SendApprovalRequest(req ApprovalRequest, approvalURL string) error
}

// --- Discord Webhook ---

// DiscordWebhookNotifier sends approval requests via Discord webhook.
type DiscordWebhookNotifier struct {
	WebhookURL string
	client     *http.Client
}

func NewDiscordWebhookNotifier(webhookURL string) *DiscordWebhookNotifier {
	return &DiscordWebhookNotifier{
		WebhookURL: webhookURL,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

func (n *DiscordWebhookNotifier) SendApprovalRequest(req ApprovalRequest, approvalURL string) error {
	// Discord embed for rich formatting
	payload := map[string]any{
		"embeds": []map[string]any{
			{
				"title":       "Approval Required",
				"description": fmt.Sprintf("**Action:** `%s`\n**Context:** %s\n**Requester:** %s", req.Action, req.Context, req.Requester),
				"color":       0xFFA500, // orange
				"fields": []map[string]any{
					{
						"name":  "Approve",
						"value": fmt.Sprintf("[Open Approval Page](%s)", approvalURL),
					},
					{
						"name":   "Expires",
						"value":  fmt.Sprintf("<t:%d:R>", req.ExpiresAt.Unix()),
						"inline": true,
					},
				},
				"timestamp": req.CreatedAt.Format(time.RFC3339),
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal discord payload: %w", err)
	}

	resp, err := n.client.Post(n.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send discord webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("discord webhook returned %d", resp.StatusCode)
	}

	log.Printf("[vault] Discord notification sent for approval %s", req.ID)
	return nil
}

// --- Telegram Bot ---

// TelegramNotifier sends approval requests via the Telegram Bot API.
type TelegramNotifier struct {
	BotToken string
	ChatID   string // Group/channel ID to send notifications to
	client   *http.Client
}

func NewTelegramNotifier(botToken, chatID string) *TelegramNotifier {
	return &TelegramNotifier{
		BotToken: botToken,
		ChatID:   chatID,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (n *TelegramNotifier) SendApprovalRequest(req ApprovalRequest, approvalURL string) error {
	text := fmt.Sprintf(
		"🔐 *Approval Required*\n\n*Action:* `%s`\n*Context:* %s\n*Requester:* %s\n*Expires:* %s\n\n[Approve / Deny](%s)",
		escapeMarkdownV2(req.Action),
		escapeMarkdownV2(req.Context),
		escapeMarkdownV2(req.Requester),
		escapeMarkdownV2(req.ExpiresAt.Format("15:04:05 UTC")),
		approvalURL,
	)

	payload := map[string]any{
		"chat_id":    n.ChatID,
		"text":       text,
		"parse_mode": "MarkdownV2",
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]any{
				{
					{"text": "✅ Approve (WebAuthn)", "url": approvalURL},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal telegram payload: %w", err)
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.BotToken)
	resp, err := n.client.Post(apiURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send telegram message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram API returned %d", resp.StatusCode)
	}

	log.Printf("[vault] Telegram notification sent for approval %s", req.ID)
	return nil
}

// escapeMarkdownV2 escapes special characters for Telegram MarkdownV2.
func escapeMarkdownV2(s string) string {
	return telegramEscaper.Replace(s)
}

var telegramEscaper = strings.NewReplacer(
	`\`, `\\`, // backslash first to avoid double-escaping
	`_`, `\_`, `*`, `\*`, `[`, `\[`, `]`, `\]`,
	`(`, `\(`, `)`, `\)`, `~`, `\~`, "`", "\\`",
	`>`, `\>`, `#`, `\#`, `+`, `\+`, `-`, `\-`,
	`=`, `\=`, `|`, `\|`, `{`, `\{`, `}`, `\}`,
	`.`, `\.`, `!`, `\!`,
)

// --- Log Notifier (fallback / development) ---

// LogNotifier logs approval requests to stdout. Used when no webhook is configured.
type LogNotifier struct{}

func (n *LogNotifier) SendApprovalRequest(req ApprovalRequest, approvalURL string) error {
	log.Printf("[vault] APPROVAL NEEDED: %s (%s) — %s", req.Action, req.Context, approvalURL)
	return nil
}

// --- Multi Notifier (fan-out) ---

// MultiNotifier sends to all notifiers. Errors are logged but don't block.
// This allows adding channels later (Discord + APNs + email) without changing callers.
type MultiNotifier struct {
	notifiers []Notifier
}

func NewMultiNotifier(notifiers ...Notifier) *MultiNotifier {
	return &MultiNotifier{notifiers: notifiers}
}

func (m *MultiNotifier) SendApprovalRequest(req ApprovalRequest, approvalURL string) error {
	var lastErr error
	succeeded := 0
	for _, n := range m.notifiers {
		if err := n.SendApprovalRequest(req, approvalURL); err != nil {
			log.Printf("[vault] Notifier %T failed: %v", n, err)
			lastErr = err
		} else {
			succeeded++
		}
	}
	// Return error only if ALL notifiers failed
	if succeeded == 0 && lastErr != nil {
		return fmt.Errorf("all notifiers failed, last error: %w", lastErr)
	}
	return nil
}
