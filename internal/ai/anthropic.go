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

type anthropicContent struct {
    Type   string                 `json:"type"`
    Text   string                 `json:"text,omitempty"`
    Source map[string]interface{} `json:"source,omitempty"`
}

type anthropicMessage struct {
    Role    string             `json:"role"`
    Content []anthropicContent `json:"content"`
}

type anthropicMsgReq struct {
    Model     string             `json:"model"`
    MaxTokens int                `json:"max_tokens"`
    System    string             `json:"system,omitempty"`
    Messages  []anthropicMessage `json:"messages"`
}

type anthropicMsgResp struct {
    Content []struct {
        Text string `json:"text"`
    } `json:"content"`
    Usage struct {
        InputTokens  int `json:"input_tokens"`
        OutputTokens int `json:"output_tokens"`
    } `json:"usage"`
}

func (c *AnthropicClient) Do(ctx context.Context, req Request) (Response, error) {
    if c.apiKey == "" {
        return Response{}, errors.New("missing ANTHROPIC_API_KEY")
    }

    // Build user message content with vision support
    var userContent []anthropicContent

    // Add image if provided (vision mode)
    if req.ImageBase64 != "" {
        userContent = append(userContent, anthropicContent{
            Type: "image",
            Source: map[string]interface{}{
                "type":       "base64",
                "media_type": req.ImageMIME,
                "data":       req.ImageBase64,
            },
        })
    }

    // Build user prompt with context and MuPDF text
    var userPrompt string

    userPrompt = fmt.Sprintf("CURRENT PAGE NUMBER: %d\n\n", req.PageID)

    if req.ContextText != "" {
        userPrompt += fmt.Sprintf("CONTEXT (from surrounding pages):\n%s\n\n", req.ContextText)
    }
    if req.MuPDFText != "" {
        userPrompt += fmt.Sprintf("MUPDF EXTRACTED TEXT (from current page):\n%s\n\n", req.MuPDFText)
    }
    userPrompt += "Please extract and return the complete text from this page following the rules in the system prompt."

    userContent = append(userContent, anthropicContent{
        Type: "text",
        Text: userPrompt,
    })

    payload := anthropicMsgReq{
        Model:     req.Model,
        MaxTokens: 4096,
        System:    req.SystemPrompt,
        Messages: []anthropicMessage{{
            Role:    "user",
            Content: userContent,
        }},
    }

    body, _ := json.Marshal(payload)
    httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
    httpReq.Header.Set("x-api-key", c.apiKey)
    httpReq.Header.Set("anthropic-version", "2023-06-01")
    httpReq.Header.Set("Content-Type", "application/json")

    resp, err := c.http.Do(httpReq)
    if err != nil {
        return Response{}, err
    }
    defer resp.Body.Close()

    if resp.StatusCode == 429 {
        return Response{}, ErrRateLimited
    }
    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        return Response{}, fmt.Errorf("anthropic status %d", resp.StatusCode)
    }

    var r anthropicMsgResp
    if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
        return Response{}, err
    }
    if len(r.Content) == 0 {
        return Response{}, errors.New("no content")
    }

    return Response{
        Text:      r.Content[0].Text,
        TokensIn:  r.Usage.InputTokens,
        TokensOut: r.Usage.OutputTokens,
    }, nil
}
