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

type openAIMessage struct {
    Role    string                   `json:"role"`
    Content []map[string]interface{} `json:"content"`
}

type openAIChatReq struct {
    Model       string          `json:"model"`
    Messages    []openAIMessage `json:"messages"`
    Temperature float64         `json:"temperature"`
    MaxTokens   int             `json:"max_tokens,omitempty"`
}

type openAIChatResp struct {
    Choices []struct {
        Message struct {
            Content string `json:"content"`
        } `json:"message"`
    } `json:"choices"`
    Usage struct {
        PromptTokens     int `json:"prompt_tokens"`
        CompletionTokens int `json:"completion_tokens"`
    } `json:"usage"`
}

func (c *OpenAIClient) Do(ctx context.Context, req Request) (Response, error) {
    if c.apiKey == "" {
        return Response{}, errors.New("missing OPENAI_API_KEY")
    }

    // Build messages with vision support
    var messages []openAIMessage

    // System message
    if req.SystemPrompt != "" {
        messages = append(messages, openAIMessage{
            Role: "system",
            Content: []map[string]interface{}{
                {"type": "text", "text": req.SystemPrompt},
            },
        })
    }

    // User message with image (if provided) and context
    var userContent []map[string]interface{}

    // Add image if provided (vision mode)
    if req.ImageBase64 != "" {
        imageURL := fmt.Sprintf("data:%s;base64,%s", req.ImageMIME, req.ImageBase64)
        userContent = append(userContent, map[string]interface{}{
            "type":      "image_url",
            "image_url": map[string]string{"url": imageURL},
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

    userContent = append(userContent, map[string]interface{}{
        "type": "text",
        "text": userPrompt,
    })

    messages = append(messages, openAIMessage{
        Role:    "user",
        Content: userContent,
    })

    payload := openAIChatReq{
        Model:       req.Model,
        Messages:    messages,
        Temperature: 0,
        MaxTokens:   4096,
    }

    body, _ := json.Marshal(payload)
    httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
    httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
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
        return Response{}, fmt.Errorf("openai status %d", resp.StatusCode)
    }

    var r openAIChatResp
    if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
        return Response{}, err
    }
    if len(r.Choices) == 0 {
        return Response{}, errors.New("no choices")
    }

    return Response{
        Text:      r.Choices[0].Message.Content,
        TokensIn:  r.Usage.PromptTokens,
        TokensOut: r.Usage.CompletionTokens,
    }, nil
}
