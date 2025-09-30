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

type OpenAIClient struct{
    http *http.Client
    apiKey string
}

func NewOpenAIClient() *OpenAIClient {
    return &OpenAIClient{http: &http.Client{}, apiKey: os.Getenv("OPENAI_API_KEY")}
}
func (c *OpenAIClient) Name() string { return "openai" }

type openAIChatReq struct {
    Model string `json:"model"`
    Messages []struct{ Role string `json:"role"`; Content []map[string]string `json:"content"` } `json:"messages"`
    Temperature float64 `json:"temperature"`
}

type openAIChatResp struct {
    Choices []struct{ Message struct{ Content string `json:"content"` } `json:"message"` } `json:"choices"`
}

func (c *OpenAIClient) Do(ctx context.Context, req Request) (Response, error) {
    if c.apiKey == "" { return Response{}, errors.New("missing OPENAI_API_KEY") }
    prompt := fmt.Sprintf("Extract clean text for page: %s", req.ContentRef)
    payload := openAIChatReq{Model: req.Model, Temperature: 0}
    msg := struct{ Role string `json:"role"`; Content []map[string]string `json:"content"` }{Role: "user", Content: []map[string]string{{"type":"text","text": prompt}}}
    payload.Messages = []struct{ Role string `json:"role"`; Content []map[string]string `json:"content"` }{msg}
    body, _ := json.Marshal(payload)
    httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
    httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
    httpReq.Header.Set("Content-Type", "application/json")
    resp, err := c.http.Do(httpReq)
    if err != nil { return Response{}, err }
    defer resp.Body.Close()
    if resp.StatusCode == 429 { return Response{}, ErrRateLimited }
    if resp.StatusCode < 200 || resp.StatusCode >= 300 { return Response{}, fmt.Errorf("openai status %d", resp.StatusCode) }
    var r openAIChatResp
    if err := json.NewDecoder(resp.Body).Decode(&r); err != nil { return Response{}, err }
    if len(r.Choices) == 0 { return Response{}, errors.New("no choices") }
    return Response{Text: r.Choices[0].Message.Content}, nil
}
