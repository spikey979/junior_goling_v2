package store

import (
    "context"
    "fmt"

    redis "github.com/redis/go-redis/v9"
)

type PageStore struct {
    client *redis.Client
}

func NewPageStore(redisURL string) (*PageStore, error) {
    opt, err := redis.ParseURL(redisURL)
    if err != nil { return nil, err }
    c := redis.NewClient(opt)
    if err := c.Ping(context.Background()).Err(); err != nil { return nil, err }
    return &PageStore{client: c}, nil
}

func (s *PageStore) Close() error { return s.client.Close() }

func (s *PageStore) pageKey(jobID string, page int) string {
    return fmt.Sprintf("job:%s:page:%d", jobID, page)
}

func (s *PageStore) SavePageText(ctx context.Context, jobID string, page int, text, source, provider, model string) error {
    m := map[string]interface{}{"text": text, "source": source}
    if provider != "" { m["provider"] = provider }
    if model != "" { m["model"] = model }
    return s.client.HSet(ctx, s.pageKey(jobID, page), m).Err()
}

func (s *PageStore) GetPageText(ctx context.Context, jobID string, page int) (string, error) {
    res, err := s.client.HGet(ctx, s.pageKey(jobID, page), "text").Result()
    if err == redis.Nil { return "", nil }
    return res, err
}

// GetPageTextWithSource returns both text and source for a page
func (s *PageStore) GetPageTextWithSource(ctx context.Context, jobID string, page int) (string, string, error) {
    key := s.pageKey(jobID, page)
    res, err := s.client.HGetAll(ctx, key).Result()
    if err != nil {
        return "", "", err
    }
    if len(res) == 0 {
        return "", "", nil
    }
    text := res["text"]
    source := res["source"]
    return text, source, nil
}

func (s *PageStore) AggregateText(ctx context.Context, jobID string, total int) (string, error) {
    out := ""
    for i := 1; i <= total; i++ {
        t, err := s.GetPageText(ctx, jobID, i)
        if err != nil { return out, err }
        if t != "" {
            if out != "" { out += "\n\n" }
            out += t
        }
    }
    return out, nil
}

