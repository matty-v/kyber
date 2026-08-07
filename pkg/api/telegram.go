package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// registerTelegramWebhook calls the Telegram Bot API to register a webhook URL
// for the given bot. After this, Telegram will POST updates for this bot to:
//
//	<publicURL>/webhooks/telegram/<agentName>
//
// The secret_token parameter tells Telegram to include an HMAC header
// (X-Telegram-Bot-Api-Secret-Token) on every webhook delivery, which the
// Kyber webhook handler verifies.
//
// This is called automatically when an agent is created with telegramEnabled +
// a bot token, so operators don't need to manually call setWebhook.
func registerTelegramWebhook(botToken, agentName, publicURL, webhookSecret string) error {
	if botToken == "" || publicURL == "" {
		return nil
	}

	webhookURL := fmt.Sprintf("%s/webhooks/telegram/%s", publicURL, agentName)
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/setWebhook", botToken)

	params := url.Values{
		"url": {webhookURL},
	}
	if webhookSecret != "" {
		params.Set("secret_token", webhookSecret)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(apiURL, params)
	if err != nil {
		return fmt.Errorf("calling Telegram setWebhook: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if jsonErr := json.Unmarshal(body, &result); jsonErr != nil {
		return fmt.Errorf("parsing Telegram response: %s", string(body))
	}

	if !result.OK {
		return fmt.Errorf("Telegram setWebhook failed: %s", result.Description)
	}

	slog.Info("telegram webhook registered",
		"agent", agentName,
		"webhook_url", webhookURL)
	return nil
}

// unregisterTelegramWebhook calls deleteWebhook for a bot when an agent is
// deleted, so Telegram stops sending updates to the now-defunct endpoint.
func unregisterTelegramWebhook(botToken string) error {
	if botToken == "" {
		return nil
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/deleteWebhook", botToken)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(apiURL, nil)
	if err != nil {
		return fmt.Errorf("calling Telegram deleteWebhook: %w", err)
	}
	defer resp.Body.Close()
	return nil
}
