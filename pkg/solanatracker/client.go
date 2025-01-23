package solanatracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/exp/rand"
)

const (
	baseURL             = "https://data.solanatracker.io"
	defaultTimeout      = 30 * time.Second
	maxRetries          = 3
	maxRateLimitRetries = 5 // Separate counter for rate limits
	baseRetryDelay      = 2 * time.Second
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

// Updated doRequestWithRetry implementation
func (c *Client) doRequestWithRetry(req *http.Request) (*http.Response, error) {
	var (
		rateLimitRetries int
		lastErr          error
	)

	for attempt := 0; attempt < maxRetries; attempt++ {
		resp, err := c.executeRequest(req)
		if err != nil {
			lastErr = err
			continue
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return c.handleRateLimit(req, resp, &rateLimitRetries)
		case resp.StatusCode >= 500:
			return c.handleServerError(resp)
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		default:
			return c.handleNonRetryableError(resp)
		}
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// New helper methods
func (c *Client) handleRateLimit(req *http.Request, resp *http.Response, retryCount *int) (*http.Response, error) {
	defer resp.Body.Close()

	if *retryCount >= maxRateLimitRetries {
		return nil, fmt.Errorf("max rate limit retries (%d) exceeded", maxRateLimitRetries)
	}

	delay := getRetryDelay(resp)
	log.Printf("Rate limited - retry %d/%d in %v",
		*retryCount+1, maxRateLimitRetries, delay)

	select {
	case <-time.After(delay):
		*retryCount++
		return c.doRequestWithRetry(req)
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

func (c *Client) executeRequest(req *http.Request) (*http.Response, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) handleServerError(resp *http.Response) (*http.Response, error) {
	defer resp.Body.Close()
	return nil, fmt.Errorf("server error: %d", resp.StatusCode)
}

func (c *Client) handleNonRetryableError(resp *http.Response) (*http.Response, error) {
	defer resp.Body.Close()
	return nil, fmt.Errorf("non-retryable error: %d", resp.StatusCode)
}

// Updated getRetryDelay with jitter
func getRetryDelay(resp *http.Response) time.Duration {
	if delay, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return time.Duration(delay)*time.Second + time.Duration(rand.Int63n(1000))*time.Millisecond
	}
	return baseRetryDelay + time.Duration(rand.Int63n(2000))*time.Millisecond
}
