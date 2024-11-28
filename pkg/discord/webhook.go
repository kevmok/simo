package discord

import (
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/webhook"
	"github.com/disgoorg/snowflake/v2"
)

type WebhookClient interface {
	SendMessage(content string) error
}

type DiscordWebhookClient struct {
	client webhook.Client
}

func NewWebhookClient(webhookIDStr, webhookToken string) (WebhookClient, error) {
	if webhookIDStr == "" || webhookToken == "" {
		return nil, fmt.Errorf("invalid webhook ID or token")
	}

	webhookID, err := snowflake.Parse(webhookIDStr)
	if err != nil {
		return nil, fmt.Errorf("invalid webhook ID: %w", err)
	}

	client := webhook.New(webhookID, webhookToken)
	return &DiscordWebhookClient{client: client}, nil
}

func ParseWebhookURL(url string) (string, string, error) {
	parts := strings.Split(url, "/")
	if len(parts) < 7 {
		return "", "", fmt.Errorf("invalid webhook URL format")
	}

	webhookID := parts[len(parts)-2]
	webhookToken := parts[len(parts)-1]

	if webhookID == "" || webhookToken == "" {
		return "", "", fmt.Errorf("webhook ID or token is empty")
	}

	return webhookID, webhookToken, nil
}

// SendMessage implements the message sending functionality
func (c *DiscordWebhookClient) SendMessage(content string) error {
	_, err := c.client.CreateMessage(discord.NewWebhookMessageCreateBuilder().SetContent(content).Build())
	return err
}
