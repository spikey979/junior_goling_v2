package orchestrator

import (
    "context"
    "github.com/local/aidispatcher/internal/store"
)

type redisStatusAdapter struct { s *store.RedisStatus }

func NewStatusAdapter(s *store.RedisStatus) StatusStore { return &redisStatusAdapter{s: s} }

func (a *redisStatusAdapter) Set(ctx context.Context, jobID string, st Status) error {
    m := make(map[string]interface{})
    if st.Metadata != nil { m = st.Metadata }
    return a.s.Set(ctx, jobID, store.Status{
        Status: st.Status,
        Progress: st.Progress,
        Message: st.Message,
        Start: st.Start,
        End: st.End,
        Metadata: m,
    })
}

func (a *redisStatusAdapter) Get(ctx context.Context, jobID string) (Status, bool, error) {
    st, ok, err := a.s.Get(ctx, jobID)
    if !ok || err != nil { return Status{}, ok, err }
    return Status{
        Status: st.Status,
        Progress: st.Progress,
        Message: st.Message,
        Start: st.Start,
        End: st.End,
        Metadata: st.Metadata,
    }, true, nil
}

