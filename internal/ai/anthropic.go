package ai

import (
    "bytes"
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "net/http"
    "os"
    "strings"
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
        Type string `json:"type"` // "text" or potentially "refusal" in future
    } `json:"content"`
    StopReason string `json:"stop_reason"` // "end_turn", "max_tokens", "stop_sequence"
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

    text := r.Content[0].Text

    // REFUSAL DETECTION 1: Check content type (future-proofing for explicit refusal type)
    if r.Content[0].Type == "refusal" {
        return Response{}, fmt.Errorf("%w: %s", ErrContentRefused, text)
    }

    // REFUSAL DETECTION 2: Heuristic - check for refusal phrases
    if isAnthropicRefusal(text) {
        return Response{}, fmt.Errorf("%w: detected refusal pattern in response", ErrContentRefused)
    }

    return Response{
        Text:      text,
        TokensIn:  r.Usage.InputTokens,
        TokensOut: r.Usage.OutputTokens,
    }, nil
}

// isAnthropicRefusal checks if Anthropic response contains refusal patterns
func isAnthropicRefusal(content string) bool {
    if len(content) < 10 {
        return false
    }

    refusalPhrases := []string{
        "i cannot assist",
        "i'm unable to help",
        "i cannot provide",
        "i cannot process",
        "i'm not able to",
        "i can't help with",
        "i'm not comfortable",
        "i must decline",
        "i should not",
        "i will not",
        "against my values",
        "not appropriate for me",
    }

    contentLower := strings.ToLower(content)
    for _, phrase := range refusalPhrases {
        if strings.Contains(contentLower, phrase) {
            return true
        }
    }

    return false
}
