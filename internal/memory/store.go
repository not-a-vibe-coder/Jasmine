package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

type HybridStore struct {
	db  *sql.DB
	rdb *redis.Client

	// In-memory fallback
	mu          sync.RWMutex
	memMessages map[int64][]Message
	memSummary  map[int64]string
	memProfiles map[int64]*UserProfile
	activeChats map[int64]bool
}

func NewHybridStore(dbURL, redisURL string) (*HybridStore, error) {
	store := &HybridStore{
		memMessages: make(map[int64][]Message),
		memSummary:  make(map[int64]string),
		memProfiles: make(map[int64]*UserProfile),
		activeChats: make(map[int64]bool),
	}

	// 1. Initialize Postgres if available
	if dbURL != "" {
		db, err := sql.Open("postgres", dbURL)
		if err != nil {
			log.Printf("[Memory] Postgres open warning: %v (falling back to memory)", err)
		} else {
			db.SetMaxOpenConns(10)
			db.SetMaxIdleConns(5)
			db.SetConnMaxLifetime(5 * time.Minute)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if err := db.PingContext(ctx); err != nil {
				log.Printf("[Memory] Postgres ping warning: %v", err)
			} else {
				store.db = db
				if err := store.initPostgresSchema(ctx); err != nil {
					log.Printf("[Memory] Postgres schema init warning: %v", err)
				} else {
					log.Printf("[Memory] Postgres database connected and schema initialized")
				}
			}
		}
	}

	// 2. Initialize Redis if available
	if redisURL != "" {
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			log.Printf("[Memory] Redis URL parse warning: %v", err)
		} else {
			rdb := redis.NewClient(opt)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := rdb.Ping(ctx).Result(); err != nil {
				log.Printf("[Memory] Redis ping warning: %v", err)
			} else {
				store.rdb = rdb
				log.Printf("[Memory] Redis cache connected successfully")
			}
		}
	}

	return store, nil
}

func (s *HybridStore) initPostgresSchema(ctx context.Context) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS chat_messages (
			id BIGSERIAL PRIMARY KEY,
			chat_id BIGINT NOT NULL,
			sender_id BIGINT NOT NULL,
			sender_username TEXT,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_chat_messages_chat_id ON chat_messages(chat_id, created_at DESC);`,
		`CREATE TABLE IF NOT EXISTS chat_summaries (
			chat_id BIGINT PRIMARY KEY,
			summary TEXT NOT NULL,
			updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		);`,
		`CREATE TABLE IF NOT EXISTS user_profiles (
			chat_id BIGINT PRIMARY KEY,
			preferences TEXT NOT NULL DEFAULT '',
			active_projects TEXT NOT NULL DEFAULT '',
			life_context TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		);`,
	}

	for _, q := range queries {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *HybridStore) SaveMessage(ctx context.Context, chatID int64, senderID int64, username, role, content string) error {
	msg := Message{
		Role:      role,
		Sender:    username,
		Content:   content,
		CreatedAt: time.Now(),
	}

	// In-memory fallback
	s.mu.Lock()
	s.memMessages[chatID] = append(s.memMessages[chatID], msg)
	if len(s.memMessages[chatID]) > 50 {
		s.memMessages[chatID] = s.memMessages[chatID][len(s.memMessages[chatID])-50:]
	}
	s.activeChats[chatID] = true
	s.mu.Unlock()

	// Redis caching
	if s.rdb != nil {
		data, err := json.Marshal(msg)
		if err == nil {
			key := fmt.Sprintf("shipp:chat:%d:messages", chatID)
			pipe := s.rdb.Pipeline()
			pipe.RPush(ctx, key, data)
			pipe.LTrim(ctx, key, -50, -1) // keep last 50
			pipe.Expire(ctx, key, 7*24*time.Hour)
			pipe.SAdd(ctx, "shipp:active_chats", chatID)
			_, _ = pipe.Exec(ctx)
		}
	}

	// Postgres persistent storage
	if s.db != nil {
		query := `INSERT INTO chat_messages (chat_id, sender_id, sender_username, role, content, created_at) VALUES ($1, $2, $3, $4, $5, $6)`
		_, err := s.db.ExecContext(ctx, query, chatID, senderID, username, role, content, msg.CreatedAt)
		if err != nil {
			log.Printf("[Memory] Postgres save error: %v", err)
			return err
		}
	}

	return nil
}

func (s *HybridStore) GetRecentMessages(ctx context.Context, chatID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}

	// 1. Try Redis
	if s.rdb != nil {
		key := fmt.Sprintf("shipp:chat:%d:messages", chatID)
		items, err := s.rdb.LRange(ctx, key, -int64(limit), -1).Result()
		if err == nil && len(items) > 0 {
			var msgs []Message
			for _, item := range items {
				var m Message
				if err := json.Unmarshal([]byte(item), &m); err == nil {
					msgs = append(msgs, m)
				}
			}
			if len(msgs) > 0 {
				return msgs, nil
			}
		}
	}

	// 2. Try Postgres
	if s.db != nil {
		query := `SELECT role, sender_username, content, created_at FROM chat_messages 
				  WHERE chat_id = $1 ORDER BY created_at DESC LIMIT $2`
		rows, err := s.db.QueryContext(ctx, query, chatID, limit)
		if err == nil {
			defer rows.Close()
			var reversed []Message
			for rows.Next() {
				var m Message
				var sender sql.NullString
				if err := rows.Scan(&m.Role, &sender, &m.Content, &m.CreatedAt); err == nil {
					if sender.Valid {
						m.Sender = sender.String
					}
					reversed = append(reversed, m)
				}
			}
			// Reverse order to chronological (oldest to newest)
			n := len(reversed)
			msgs := make([]Message, n)
			for i, m := range reversed {
				msgs[n-1-i] = m
			}
			return msgs, nil
		}
	}

	// 3. Fallback to In-Memory
	s.mu.RLock()
	defer s.mu.RUnlock()
	inMem := s.memMessages[chatID]
	if len(inMem) == 0 {
		return []Message{}, nil
	}
	if len(inMem) > limit {
		return inMem[len(inMem)-limit:], nil
	}
	return inMem, nil
}

func (s *HybridStore) ClearContext(ctx context.Context, chatID int64) error {
	// In-memory
	s.mu.Lock()
	delete(s.memMessages, chatID)
	delete(s.memSummary, chatID)
	s.mu.Unlock()

	// Redis
	if s.rdb != nil {
		key := fmt.Sprintf("shipp:chat:%d:messages", chatID)
		sumKey := fmt.Sprintf("shipp:chat:%d:summary", chatID)
		_ = s.rdb.Del(ctx, key, sumKey).Err()
	}

	// Postgres
	if s.db != nil {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM chat_messages WHERE chat_id = $1`, chatID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM chat_summaries WHERE chat_id = $1`, chatID)
	}

	return nil
}

func (s *HybridStore) SaveSummary(ctx context.Context, chatID int64, summary string) error {
	// In-memory
	s.mu.Lock()
	s.memSummary[chatID] = summary
	s.mu.Unlock()

	// Redis
	if s.rdb != nil {
		key := fmt.Sprintf("shipp:chat:%d:summary", chatID)
		_ = s.rdb.Set(ctx, key, summary, 30*24*time.Hour).Err()
	}

	// Postgres
	if s.db != nil {
		query := `INSERT INTO chat_summaries (chat_id, summary, updated_at) 
				  VALUES ($1, $2, NOW()) 
				  ON CONFLICT (chat_id) 
				  DO UPDATE SET summary = EXCLUDED.summary, updated_at = NOW()`
		_, err := s.db.ExecContext(ctx, query, chatID, summary)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *HybridStore) GetSummary(ctx context.Context, chatID int64) (string, error) {
	// Redis
	if s.rdb != nil {
		key := fmt.Sprintf("shipp:chat:%d:summary", chatID)
		summary, err := s.rdb.Get(ctx, key).Result()
		if err == nil && summary != "" {
			return summary, nil
		}
	}

	// Postgres
	if s.db != nil {
		var summary string
		err := s.db.QueryRowContext(ctx, `SELECT summary FROM chat_summaries WHERE chat_id = $1`, chatID).Scan(&summary)
		if err == nil {
			return summary, nil
		}
	}

	// In-memory
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.memSummary[chatID], nil
}

func (s *HybridStore) SaveUserProfile(ctx context.Context, profile UserProfile) error {
	profile.UpdatedAt = time.Now()

	// In-memory
	s.mu.Lock()
	s.memProfiles[profile.ChatID] = &profile
	s.mu.Unlock()

	// Redis
	if s.rdb != nil {
		key := fmt.Sprintf("shipp:chat:%d:profile", profile.ChatID)
		if data, err := json.Marshal(profile); err == nil {
			_ = s.rdb.Set(ctx, key, data, 90*24*time.Hour).Err()
		}
	}

	// Postgres
	if s.db != nil {
		query := `INSERT INTO user_profiles (chat_id, preferences, active_projects, life_context, updated_at) 
				  VALUES ($1, $2, $3, $4, NOW()) 
				  ON CONFLICT (chat_id) 
				  DO UPDATE SET preferences = EXCLUDED.preferences, 
				                active_projects = EXCLUDED.active_projects, 
				                life_context = EXCLUDED.life_context, 
				                updated_at = NOW()`
		_, err := s.db.ExecContext(ctx, query, profile.ChatID, profile.Preferences, profile.ActiveProjects, profile.LifeContext)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *HybridStore) GetUserProfile(ctx context.Context, chatID int64) (*UserProfile, error) {
	// Redis
	if s.rdb != nil {
		key := fmt.Sprintf("shipp:chat:%d:profile", chatID)
		data, err := s.rdb.Get(ctx, key).Bytes()
		if err == nil && len(data) > 0 {
			var p UserProfile
			if err := json.Unmarshal(data, &p); err == nil {
				return &p, nil
			}
		}
	}

	// Postgres
	if s.db != nil {
		var p UserProfile
		p.ChatID = chatID
		err := s.db.QueryRowContext(ctx, `SELECT preferences, active_projects, life_context, updated_at FROM user_profiles WHERE chat_id = $1`, chatID).
			Scan(&p.Preferences, &p.ActiveProjects, &p.LifeContext, &p.UpdatedAt)
		if err == nil {
			return &p, nil
		}
	}

	// In-memory
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.memProfiles[chatID]; ok {
		return p, nil
	}
	return nil, nil
}

func (s *HybridStore) GetActiveChatIDs(ctx context.Context) ([]int64, error) {
	chatMap := make(map[int64]bool)

	// Redis
	if s.rdb != nil {
		members, err := s.rdb.SMembers(ctx, "shipp:active_chats").Result()
		if err == nil {
			for _, m := range members {
				if id, err := strconv.ParseInt(m, 10, 64); err == nil {
					chatMap[id] = true
				}
			}
		}
	}

	// Postgres
	if s.db != nil {
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT chat_id FROM chat_messages ORDER BY chat_id`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err == nil {
					chatMap[id] = true
				}
			}
		}
	}

	// In-memory
	s.mu.RLock()
	for id := range s.activeChats {
		chatMap[id] = true
	}
	s.mu.RUnlock()

	var result []int64
	for id := range chatMap {
		result = append(result, id)
	}
	return result, nil
}

func (s *HybridStore) GetDB() *sql.DB {
	return s.db
}

func (s *HybridStore) GetRedis() *redis.Client {
	return s.rdb
}

func (s *HybridStore) Close() error {
	if s.db != nil {
		_ = s.db.Close()
	}
	if s.rdb != nil {
		_ = s.rdb.Close()
	}
	return nil
}
