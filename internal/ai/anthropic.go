package ai

import (
    "bytes"
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "net/http"
    "os"
)

type AnthropicClient struct{ http *http.Client; apiKey string }

func NewAnthropicClient() *AnthropicClient { return &AnthropicClient{http: &http.Client{}, apiKey: os.Getenv("ANTHROPIC_API_KEY")} }
func (c *AnthropicClient) Name() string { return "anthropic" }

type anthropicMsgReq struct {
    Model string `json:"model"`
    MaxTokens int `json:"max_tokens"`
    Messages []struct{ Role string `json:"role"`; Content string `json:"content"` } `json:"messages"`
}

type anthropicMsgResp struct { Content []struct{ Text string `json:"text"` } `json:"content"` }

func (c *AnthropicClient) Do(ctx context.Context, req Request) (Response, error) {
    if c.apiKey == "" { return Response{}, errors.New("missing ANTHROPIC_API_KEY") }
    prompt := fmt.Sprintf("Extract clean text for page: %s", req.ContentRef)
    payload := anthropicMsgReq{Model: req.Model, MaxTokens: 1024}
    payload.Messages = []struct{ Role string `json:"role"`; Content string `json:"content"` }{{Role: "user", Content: prompt}}
    body, _ := json.Marshal(payload)
    httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
    httpReq.Header.Set("x-api-key", c.apiKey)
    httpReq.Header.Set("anthropic-version", "2023-06-01")
    httpReq.Header.Set("Content-Type", "application/json")
    resp, err := c.http.Do(httpReq)
    if err != nil { return Response{}, err }
    defer resp.Body.Close()
    if resp.StatusCode == 429 { return Response{}, ErrRateLimited }
    if resp.StatusCode < 200 || resp.StatusCode >= 300 { return Response{}, fmt.Errorf("anthropic status %d", resp.StatusCode) }
    var r anthropicMsgResp
    if err := json.NewDecoder(resp.Body).Decode(&r); err != nil { return Response{}, err }
    if len(r.Content) == 0 { return Response{}, errors.New("no content") }
    return Response{Text: r.Content[0].Text}, nil
}
