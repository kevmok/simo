package solanatracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	baseURL           = "https://data.solanatracker.io"
	defaultTimeout    = 30 * time.Second
	maxRetries        = 3
	defaultRetryDelay = 3 * time.Second
)

type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

type ClientConfig struct {
	APIKey string
}

type TokenInfo struct {
	Token struct {
		Name        string `json:"name"`
		Symbol      string `json:"symbol"`
		Mint        string `json:"mint"`
		Decimals    int    `json:"decimals"`
		Image       string `json:"image"`
		Description string `json:"description"`
		Extensions  struct {
			Twitter  string `json:"twitter"`
			Telegram string `json:"telegram"`
		} `json:"extensions"`
		Creator struct {
			Name string `json:"name"`
			Site string `json:"site"`
		} `json:"creator"`
	} `json:"token"`
	Pools []struct {
		Liquidity struct {
			Quote float64 `json:"quote"`
			USD   float64 `json:"usd"`
		} `json:"liquidity"`
		Price struct {
			Quote float64 `json:"quote"`
			USD   float64 `json:"usd"`
		} `json:"price"`
		Market      string `json:"market"`
		LastUpdated int64  `json:"lastUpdated"`
		MarketCap   struct {
			Quote float64 `json:"quote"`
			USD   float64 `json:"usd"`
		} `json:"marketCap"`
	} `json:"pools"`
	Events struct {
		OneHour struct {
			PriceChangePercentage float64 `json:"priceChangePercentage"`
		} `json:"1h"`
		TwentyFourHour struct {
			PriceChangePercentage float64 `json:"priceChangePercentage"`
		} `json:"24h"`
	} `json:"events"`
	Risk struct {
		Rugged bool `json:"rugged"`
		Risks  []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Level       string `json:"level"`
			Score       int    `json:"score"`
		} `json:"risks"`
		Score int `json:"score"`
	} `json:"risk"`
}

type ProfitLoss struct {
	Holding       float64 `json:"holding"`
	Held          float64 `json:"held"`
	Sold          float64 `json:"sold"`
	Realized      float64 `json:"realized"`
	Unrealized    float64 `json:"unrealized"`
	Total         float64 `json:"total"`
	TotalSold     float64 `json:"total_sold"`
	TotalInvested float64 `json:"total_invested"`
	AvgBuyAmount  float64 `json:"average_buy_amount"`
	CurrentValue  float64 `json:"current_value"`
	CostBasis     float64 `json:"cost_basis"`
}

func NewClient(config ClientConfig) (*Client, error) {
	// Validate and sanitize API key
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("empty API key provided")
	}

	// Check for invalid characters
	if strings.ContainsAny(apiKey, "\n\r\t") {
		return nil, fmt.Errorf("API key contains invalid characters")
	}

	return &Client{
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
		baseURL: baseURL,
		apiKey:  apiKey,
	}, nil
}

func (c *Client) GetTokenInfo(ctx context.Context, tokenAddress string) (*TokenInfo, error) {
	url := fmt.Sprintf("%s/tokens/%s", c.baseURL, tokenAddress)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	// Add API key to header with additional validation
	if c.apiKey == "" {
		return nil, fmt.Errorf("API key not set")
	}

	// Add API key to header
	req.Header.Set("x-api-key", c.apiKey)

	resp, err := c.doRequestWithRetry(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var tokenInfo TokenInfo
	if err := json.NewDecoder(resp.Body).Decode(&tokenInfo); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &tokenInfo, nil
}

func (c *Client) GetProfitLoss(ctx context.Context, walletAddress, tokenAddress string) (*ProfitLoss, error) {
	url := fmt.Sprintf("%s/pnl/%s/%s", c.baseURL, walletAddress, tokenAddress)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("x-api-key", c.apiKey)
	resp, err := c.doRequestWithRetry(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var pnl ProfitLoss
	if err := json.NewDecoder(resp.Body).Decode(&pnl); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &pnl, nil
}

func (c *Client) doRequestWithRetry(req *http.Request) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			time.Sleep(backoff)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		// Handle different status codes
		switch resp.StatusCode {
		case http.StatusOK:
			return resp, nil
		case http.StatusTooManyRequests:
			// Get retry delay from response headers
			delay := getRetryDelay(resp)
			resp.Body.Close()

			// Check if we have retries left
			if attempt == maxRetries {
				return nil, fmt.Errorf("rate limit exceeded and out of retries")
			}

			// Log rate limit hit (you can modify this based on your logging setup)
			fmt.Printf("Rate limit hit, waiting %v before retry\n", delay)

			// Wait for the specified delay
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(delay):
				continue
			}

		case http.StatusUnauthorized,
			http.StatusForbidden,
			http.StatusNotFound:
			// Don't retry on these status codes
			return resp, nil

		default:
			resp.Body.Close()
			lastErr = fmt.Errorf("unexpected status code: %d", resp.StatusCode)
			continue
		}
	}

	return nil, fmt.Errorf("all retry attempts failed: %w", lastErr)
}

func getRetryDelay(resp *http.Response) time.Duration {
	retryAfter := resp.Header.Get("Retry-After")
	if retryAfter == "" {
		return defaultRetryDelay
	}

	// Try to parse as seconds
	if seconds, err := strconv.Atoi(retryAfter); err == nil {
		return time.Duration(seconds) * time.Second
	}

	// Try to parse as HTTP date
	if date, err := time.Parse(time.RFC1123, retryAfter); err == nil {
		return time.Until(date)
	}

	return defaultRetryDelay
}
