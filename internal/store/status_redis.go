package store

import (
    "context"
    "encoding/json"
    "fmt"
    "time"

    redis "github.com/redis/go-redis/v9"
)

type Status struct {
    Status   string                 `json:"status"`
    Progress int                    `json:"progress"`
    Message  string                 `json:"message"`
    Start    *time.Time             `json:"start_time,omitempty"`
    End      *time.Time             `json:"end_time,omitempty"`
    Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type RedisStatus struct {
    client *redis.Client
    keyNS  string
}

func NewRedisStatus(redisURL string) (*RedisStatus, error) {
    opt, err := redis.ParseURL(redisURL)
    if err != nil { return nil, err }
    c := redis.NewClient(opt)
    if err := c.Ping(context.Background()).Err(); err != nil { return nil, err }
    return &RedisStatus{client: c, keyNS: "job"}, nil
}

func (s *RedisStatus) key(jobID string) string { return fmt.Sprintf("%s:%s:status", s.keyNS, jobID) }

func (s *RedisStatus) Set(ctx context.Context, jobID string, st Status) error {
    m := map[string]interface{}{
        "status":   st.Status,
        "progress": st.Progress,
        "message":  st.Message,
    }
    if st.Start != nil { m["start"] = st.Start.Format(time.RFC3339Nano) }
    if st.End != nil { m["end"] = st.End.Format(time.RFC3339Nano) }
    if st.Metadata != nil {
        b, _ := json.Marshal(st.Metadata)
        m["metadata"] = string(b)
    }
    return s.client.HSet(ctx, s.key(jobID), m).Err()
}

func (s *RedisStatus) Get(ctx context.Context, jobID string) (Status, bool, error) {
    res, err := s.client.HGetAll(ctx, s.key(jobID)).Result()
    if err != nil { return Status{}, false, err }
    if len(res) == 0 { return Status{}, false, nil }
    st := Status{}
    st.Status = res["status"]
    st.Message = res["message"]
    if p, ok := res["progress"]; ok && p != "" {
        // ignore parse error; default 0
        var pi int
        fmt.Sscan(p, &pi)
        st.Progress = pi
    }
    if v := res["start"]; v != "" {
        if t, err := time.Parse(time.RFC3339Nano, v); err == nil { st.Start = &t }
    }
    if v := res["end"]; v != "" {
        if t, err := time.Parse(time.RFC3339Nano, v); err == nil { st.End = &t }
    }
    if v := res["metadata"]; v != "" {
        _ = json.Unmarshal([]byte(v), &st.Metadata)
    }
    return st, true, nil
}

func (s *RedisStatus) Close() error { return s.client.Close() }

// Client returns the underlying Redis client
func (s *RedisStatus) Client() *redis.Client { return s.client }

// SetFileJobMapping creates a mapping from file_id to job_id
func (s *RedisStatus) SetFileJobMapping(ctx context.Context, fileID, jobID string) error {
    key := fmt.Sprintf("file_to_job:%s", fileID)
    // Set with 7 day expiration (job should complete before this)
    return s.client.Set(ctx, key, jobID, 7*24*time.Hour).Err()
}

// GetJobByFileID retrieves the job_id associated with a file_id
func (s *RedisStatus) GetJobByFileID(ctx context.Context, fileID string) (string, error) {
    key := fmt.Sprintf("file_to_job:%s", fileID)
    jobID, err := s.client.Get(ctx, key).Result()
    if err == redis.Nil {
        return "", fmt.Errorf("no job found for file_id: %s", fileID)
    }
    return jobID, err
}
