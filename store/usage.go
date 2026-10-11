package store

import (
	"fmt"
)

// Usage 是 usage 表的一行(计量流水,只增不改)。
type Usage struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	TokenID    int64  `json:"token_id"`
	ChannelID  *int64 `json:"channel_id,omitempty"` // 兜底场景为 nil
	RequestID  string `json:"request_id"`
	Model      string `json:"model"`
	Characters int    `json:"characters"`
	AudioMS    int64  `json:"audio_ms"`
	AudioBytes int    `json:"audio_bytes"`
	CreatedAt  string `json:"created_at"`
}

// UsageInsert 写一条流水;channel_id 为 nil 时落 NULL(走兜底)。
// 写失败不阻断主请求(由 controller 决定 log warn,见 3a 决策 #7)。
func (s *Store) UsageInsert(u Usage) (int64, error) {
	if u.UserID == 0 || u.TokenID == 0 {
		return 0, fmt.Errorf("%w: user_id and token_id are required", ErrInvalid)
	}
	if u.RequestID == "" {
		return 0, fmt.Errorf("%w: request_id is required", ErrInvalid)
	}
	var channelVal any
	if u.ChannelID != nil {
		channelVal = *u.ChannelID
	} else {
		channelVal = nil
	}
	res, err := s.db.Exec(`
		INSERT INTO usage (user_id, token_id, channel_id, request_id, model, characters, audio_ms, audio_bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		u.UserID, u.TokenID, channelVal, u.RequestID, u.Model, u.Characters, u.AudioMS, u.AudioBytes)
	if err != nil {
		return 0, fmt.Errorf("store: usage insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: usage insert lastid: %w", err)
	}
	return id, nil
}

// UsageListByUser 按 user_id 列流水;ORDER BY id DESC(最新在前);LIMIT/OFFSET 分页。
// 决策 #9:limit/offset 形式,跟项目其它 CRUD 一致。
func (s *Store) UsageListByUser(userID int64, limit, offset int) ([]Usage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(`SELECT id, user_id, token_id, channel_id, request_id, model, characters, audio_ms, audio_bytes, created_at
	                        FROM usage WHERE user_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`,
		userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: usage list by user id=%d: %w", userID, err)
	}
	defer rows.Close()
	out := make([]Usage, 0, 16)
	for rows.Next() {
		var u Usage
		var channelID *int64
		if err := rows.Scan(&u.ID, &u.UserID, &u.TokenID, &channelID, &u.RequestID, &u.Model,
			&u.Characters, &u.AudioMS, &u.AudioBytes, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: usage list scan: %w", err)
		}
		u.ChannelID = channelID
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage list rows: %w", err)
	}
	return out, nil
}

// UsageCountByUser 统计该 user 的总流水行数(分页辅助)。
func (s *Store) UsageCountByUser(userID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM usage WHERE user_id = ?`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: usage count by user id=%d: %w", userID, err)
	}
	return n, nil
}

// UsageTotalsByUser 聚合该 user 的 characters / audio_ms / audio_bytes 总和。
// 管理端仪表盘展示用。
type UsageTotals struct {
	Characters int64 `json:"characters"`
	AudioMS    int64 `json:"audio_ms"`
	AudioBytes int64 `json:"audio_bytes"`
}

func (s *Store) UsageTotalsByUser(userID int64) (UsageTotals, error) {
	var t UsageTotals
	err := s.db.QueryRow(`SELECT COALESCE(SUM(characters),0), COALESCE(SUM(audio_ms),0), COALESCE(SUM(audio_bytes),0)
	                      FROM usage WHERE user_id = ?`, userID).
		Scan(&t.Characters, &t.AudioMS, &t.AudioBytes)
	if err != nil {
		return t, fmt.Errorf("store: usage totals by user id=%d: %w", userID, err)
	}
	return t, nil
}
