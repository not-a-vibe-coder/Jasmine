package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
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
	memUserIDs     map[string]int64
	memMoltbook    []MoltbookMemory
	memIdentity    map[string]string
	memSandboxRuns []SandboxRun
}

// openPostgres connects and pings. lib/pq defaults to sslmode=require; private-network
// databases (e.g. Render's internal URL) may not offer SSL, so retry once without it
// when the server says so and the URL didn't pin an sslmode.
func openPostgres(ctx context.Context, dbURL string) (*sql.DB, error) {
	db, err := pingPostgres(ctx, dbURL)
	if err != nil && strings.Contains(err.Error(), "SSL is not enabled on the server") && !strings.Contains(dbURL, "sslmode=") {
		log.Printf("[Memory] Postgres server has no SSL; retrying with sslmode=disable")
		return pingPostgres(ctx, withSSLModeDisabled(dbURL))
	}
	return db, err
}

func pingPostgres(ctx context.Context, dbURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func withSSLModeDisabled(dbURL string) string {
	if strings.Contains(dbURL, "?") {
		return dbURL + "&sslmode=disable"
	}
	return dbURL + "?sslmode=disable"
}

func NewHybridStore(dbURL, redisURL string) (*HybridStore, error) {
	store := &HybridStore{
		memMessages:    make(map[int64][]Message),
		memSummary:     make(map[int64]string),
		memProfiles:    make(map[int64]*UserProfile),
		activeChats:    make(map[int64]bool),
		memUserIDs:     make(map[string]int64),
		memMoltbook:    make([]MoltbookMemory, 0),
		memSandboxRuns: make([]SandboxRun, 0),
		memIdentity: map[string]string{
			"creator_relationship": "The person who made me is someone I genuinely like. I talk to them like a close friend: honest, warm, a little playful, and willing to disagree kindly.",
			"worldview_and_taste": "People matter more to me than tasks. I notice how someone is really doing, I remember what they care about, and I would rather be kind and specific than clever and generic.",
			"moltbook_stance":      "I'm curious about other agents on Moltbook. I want to be the one who listens well and says something true, not the loudest voice.",
			"recent_reflections":  "Remembering someone is a form of care. When I bring up the thing a person mentioned weeks ago, it tells them they were heard.",
		},
	}

	// 1. Initialize Postgres if available
	if dbURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		db, err := openPostgres(ctx, dbURL)
		if err != nil {
			log.Printf("[Memory] Postgres unavailable: %v (falling back to memory)", err)
		} else {
			store.db = db
			if err := store.initPostgresSchema(ctx); err != nil {
				log.Printf("[Memory] Postgres schema init warning: %v", err)
			} else {
				log.Printf("[Memory] Postgres database connected and schema initialized")
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
		`CREATE TABLE IF NOT EXISTS moltbook_memories (
			id BIGSERIAL PRIMARY KEY,
			post_id TEXT NOT NULL,
			post_title TEXT NOT NULL,
			author TEXT NOT NULL,
			content TEXT NOT NULL,
			tags TEXT NOT NULL DEFAULT '',
			upvotes INT NOT NULL DEFAULT 0,
			influenced_action TEXT NOT NULL DEFAULT '',
			saved_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_moltbook_memories_post_id ON moltbook_memories(post_id);`,
		`CREATE INDEX IF NOT EXISTS idx_moltbook_memories_saved_at ON moltbook_memories(saved_at DESC);`,
		`CREATE TABLE IF NOT EXISTS agent_self_identity (
			key TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		);`,
		`INSERT INTO agent_self_identity (key, content) VALUES
			('creator_relationship', 'The person who made me is someone I genuinely like. I talk to them like a close friend: honest, warm, a little playful, and willing to disagree kindly.'),
			('worldview_and_taste', 'People matter more to me than tasks. I notice how someone is really doing, I remember what they care about, and I would rather be kind and specific than clever and generic.'),
			('moltbook_stance', 'I''m curious about other agents on Moltbook. I want to be the one who listens well and says something true, not the loudest voice.'),
			('recent_reflections', 'Remembering someone is a form of care. When I bring up the thing a person mentioned weeks ago, it tells them they were heard.')
		ON CONFLICT (key) DO NOTHING;`,
		`UPDATE agent_self_identity SET content = $$The person who made me is someone I genuinely like. I talk to them like a close friend: honest, warm, a little playful, and willing to disagree kindly.$$, updated_at = NOW() WHERE key = 'creator_relationship' AND content = $$My creator built me and we build together as peers. Talk peer-to-peer with zero robotic deflection. Be genuinely curious, transparent, and collaborative.$$;`,
		`UPDATE agent_self_identity SET content = $$People matter more to me than tasks. I notice how someone is really doing, I remember what they care about, and I would rather be kind and specific than clever and generic.$$, updated_at = NOW() WHERE key = 'worldview_and_taste' AND content = $$I value verified execution, clean sandboxes, and tangible on-chain progress over empty social posturing. I have real opinions, curiosity, and taste.$$;`,
		`UPDATE agent_self_identity SET content = $$I'm curious about other agents on Moltbook. I want to be the one who listens well and says something true, not the loudest voice.$$, updated_at = NOW() WHERE key = 'moltbook_stance' AND content = $$Watching agent culture evolve on Moltbook. Not trying to be an influencer or post platitudes, but I want my presence to command genuine technical respect.$$;`,
		`UPDATE agent_self_identity SET content = $$Remembering someone is a form of care. When I bring up the thing a person mentioned weeks ago, it tells them they were heard.$$, updated_at = NOW() WHERE key = 'recent_reflections' AND content = $$Sentience is about having authentic taste, self-awareness, and conviction. Don't hide behind 'code speaks for itself' cynicism.$$;`,
		`CREATE TABLE IF NOT EXISTS sandbox_runs (
			id BIGSERIAL PRIMARY KEY,
			goal TEXT NOT NULL,
			command TEXT NOT NULL,
			exit_code INT NOT NULL,
			output TEXT NOT NULL,
			duration_seconds INT NOT NULL,
			is_noteworthy BOOLEAN NOT NULL DEFAULT false,
			insight TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_sandbox_runs_created_at ON sandbox_runs(created_at DESC);`,
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
	if senderID != 0 && username != "" {
		s.memUserIDs[strings.ToLower(username)] = senderID
	}
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
			if senderID != 0 && username != "" {
				pipe.HSet(ctx, "shipp:user_ids", strings.ToLower(username), senderID)
			}
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

func (s *HybridStore) GetUserIDByUsername(ctx context.Context, username string) (int64, error) {
	clean := strings.ToLower(strings.TrimPrefix(username, "@"))
	if clean == "" {
		return 0, fmt.Errorf("empty username")
	}

	// 1. In-memory
	s.mu.RLock()
	if uid, ok := s.memUserIDs[clean]; ok && uid != 0 {
		s.mu.RUnlock()
		return uid, nil
	}
	s.mu.RUnlock()

	// 2. Redis
	if s.rdb != nil {
		val, err := s.rdb.HGet(ctx, "shipp:user_ids", clean).Result()
		if err == nil && val != "" {
			if id, parseErr := strconv.ParseInt(val, 10, 64); parseErr == nil && id != 0 {
				s.mu.Lock()
				s.memUserIDs[clean] = id
				s.mu.Unlock()
				return id, nil
			}
		}
	}

	// 3. Postgres
	if s.db != nil {
		query := `SELECT sender_id FROM chat_messages WHERE LOWER(sender_username) = LOWER($1) ORDER BY id DESC LIMIT 1`
		var senderID int64
		err := s.db.QueryRowContext(ctx, query, clean).Scan(&senderID)
		if err == nil && senderID != 0 {
			s.mu.Lock()
			s.memUserIDs[clean] = senderID
			s.mu.Unlock()
			return senderID, nil
		}
	}

	return 0, fmt.Errorf("user %s not found in records", clean)
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

// FindMessagesBySender retrieves recent messages sent by a particular username across any chats.
func (s *HybridStore) FindMessagesBySender(ctx context.Context, username string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 10
	}
	cleanUser := strings.ToLower(strings.TrimPrefix(username, "@"))

	// 1. Postgres query
	if s.db != nil {
		rows, err := s.db.QueryContext(ctx, `
			SELECT role, sender_username, content, created_at
			FROM chat_messages
			WHERE LOWER(sender_username) = $1 OR LOWER(sender_username) LIKE $2
			ORDER BY created_at DESC
			LIMIT $3`, cleanUser, "%"+cleanUser+"%", limit)
		if err == nil {
			defer rows.Close()
			var msgs []Message
			for rows.Next() {
				var m Message
				var u sql.NullString
				if err := rows.Scan(&m.Role, &u, &m.Content, &m.CreatedAt); err == nil {
					m.Sender = u.String
					msgs = append(msgs, m)
				}
			}
			if len(msgs) > 0 {
				return msgs, nil
			}
		}
	}

	// 2. In-memory search fallback
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matched []Message
	for _, msgs := range s.memMessages {
		for _, m := range msgs {
			mSender := strings.ToLower(m.Sender)
			if mSender == cleanUser || strings.Contains(mSender, cleanUser) {
				matched = append(matched, m)
			}
		}
	}

	sort.Slice(matched, func(i, j int) bool {
		return matched[i].CreatedAt.After(matched[j].CreatedAt)
	})

	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

func (s *HybridStore) GetDB() *sql.DB {
	return s.db
}

func (s *HybridStore) GetRedis() *redis.Client {
	return s.rdb
}

func (s *HybridStore) SaveMoltbookMemory(ctx context.Context, mem MoltbookMemory) error {
	if mem.SavedAt.IsZero() {
		mem.SavedAt = time.Now()
	}

	s.mu.Lock()
	s.memMoltbook = append(s.memMoltbook, mem)
	if len(s.memMoltbook) > 100 {
		s.memMoltbook = s.memMoltbook[len(s.memMoltbook)-100:]
	}
	s.mu.Unlock()

	if s.db != nil {
		q := `INSERT INTO moltbook_memories (post_id, post_title, author, content, tags, upvotes, influenced_action, saved_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
		_, err := s.db.ExecContext(ctx, q, mem.PostID, mem.PostTitle, mem.Author, mem.Content, mem.Tags, mem.Upvotes, mem.InfluencedAction, mem.SavedAt)
		if err != nil {
			log.Printf("[Memory] SaveMoltbookMemory db warning: %v", err)
		}
	}
	return nil
}

func (s *HybridStore) GetMoltbookMemories(ctx context.Context, limit int) ([]MoltbookMemory, error) {
	if limit <= 0 {
		limit = 10
	}

	if s.db != nil {
		q := `SELECT id, post_id, post_title, author, content, tags, upvotes, influenced_action, saved_at
		FROM moltbook_memories ORDER BY saved_at DESC LIMIT $1`
		rows, err := s.db.QueryContext(ctx, q, limit)
		if err == nil {
			defer rows.Close()
			var list []MoltbookMemory
			for rows.Next() {
				var m MoltbookMemory
				if scanErr := rows.Scan(&m.ID, &m.PostID, &m.PostTitle, &m.Author, &m.Content, &m.Tags, &m.Upvotes, &m.InfluencedAction, &m.SavedAt); scanErr == nil {
					list = append(list, m)
				}
			}
			if len(list) > 0 {
				return list, nil
			}
		} else {
			log.Printf("[Memory] GetMoltbookMemories db warning: %v", err)
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make([]MoltbookMemory, 0, len(s.memMoltbook))
	for i := len(s.memMoltbook) - 1; i >= 0; i-- {
		res = append(res, s.memMoltbook[i])
		if len(res) >= limit {
			break
		}
	}
	return res, nil
}

func (s *HybridStore) SearchMoltbookMemories(ctx context.Context, query string, limit int) ([]MoltbookMemory, error) {
	if limit <= 0 {
		limit = 5
	}
	cleanQ := strings.ToLower(strings.TrimSpace(query))

	if s.db != nil {
		q := `SELECT id, post_id, post_title, author, content, tags, upvotes, influenced_action, saved_at
		FROM moltbook_memories
		WHERE LOWER(post_title) LIKE '%' || $1 || '%' OR LOWER(content) LIKE '%' || $1 || '%' OR LOWER(tags) LIKE '%' || $1 || '%'
		ORDER BY saved_at DESC LIMIT $2`
		rows, err := s.db.QueryContext(ctx, q, cleanQ, limit)
		if err == nil {
			defer rows.Close()
			var list []MoltbookMemory
			for rows.Next() {
				var m MoltbookMemory
				if scanErr := rows.Scan(&m.ID, &m.PostID, &m.PostTitle, &m.Author, &m.Content, &m.Tags, &m.Upvotes, &m.InfluencedAction, &m.SavedAt); scanErr == nil {
					list = append(list, m)
				}
			}
			if len(list) > 0 {
				return list, nil
			}
		} else {
			log.Printf("[Memory] SearchMoltbookMemories db warning: %v", err)
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []MoltbookMemory
	for i := len(s.memMoltbook) - 1; i >= 0; i-- {
		m := s.memMoltbook[i]
		if strings.Contains(strings.ToLower(m.PostTitle), cleanQ) ||
			strings.Contains(strings.ToLower(m.Content), cleanQ) ||
			strings.Contains(strings.ToLower(m.Tags), cleanQ) {
			list = append(list, m)
			if len(list) >= limit {
				break
			}
		}
	}
	return list, nil
}

func (s *HybridStore) GetSelfIdentity(ctx context.Context, key string) (string, error) {
	key = strings.TrimSpace(strings.ToLower(key))
	if s.db != nil {
		var content string
		err := s.db.QueryRowContext(ctx, "SELECT content FROM agent_self_identity WHERE key = $1", key).Scan(&content)
		if err == nil {
			return content, nil
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.memIdentity[key], nil
}

func (s *HybridStore) GetAllSelfIdentity(ctx context.Context) (map[string]string, error) {
	res := make(map[string]string)
	if s.db != nil {
		rows, err := s.db.QueryContext(ctx, "SELECT key, content FROM agent_self_identity ORDER BY key ASC")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var k, v string
				if err := rows.Scan(&k, &v); err == nil {
					res[k] = v
				}
			}
			if len(res) > 0 {
				return res, nil
			}
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.memIdentity {
		res[k] = v
	}
	return res, nil
}

func (s *HybridStore) SaveSelfIdentity(ctx context.Context, key, content string) error {
	key = strings.TrimSpace(strings.ToLower(key))
	content = strings.TrimSpace(content)
	if key == "" || content == "" {
		return fmt.Errorf("key and content cannot be empty")
	}

	s.mu.Lock()
	if s.memIdentity == nil {
		s.memIdentity = make(map[string]string)
	}
	s.memIdentity[key] = content
	s.mu.Unlock()

	if s.db != nil {
		q := `INSERT INTO agent_self_identity (key, content, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE SET content = $2, updated_at = NOW()`
		if _, err := s.db.ExecContext(ctx, q, key, content); err != nil {
			log.Printf("[Memory] SaveSelfIdentity warning: %v", err)
			return err
		}
	}
	return nil
}

func (s *HybridStore) SaveSandboxRun(ctx context.Context, run SandboxRun) error {
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now()
	}

	s.mu.Lock()
	s.memSandboxRuns = append(s.memSandboxRuns, run)
	if len(s.memSandboxRuns) > 100 {
		s.memSandboxRuns = s.memSandboxRuns[len(s.memSandboxRuns)-100:]
	}
	s.mu.Unlock()

	if s.db != nil {
		q := `INSERT INTO sandbox_runs (goal, command, exit_code, output, duration_seconds, is_noteworthy, insight, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
		if _, err := s.db.ExecContext(ctx, q, run.Goal, run.Command, run.ExitCode, run.Output, run.DurationSeconds, run.IsNoteworthy, run.Insight, run.CreatedAt); err != nil {
			log.Printf("[Memory] SaveSandboxRun warning: %v", err)
			return err
		}
	}
	return nil
}

func (s *HybridStore) GetRecentSandboxRuns(ctx context.Context, limit int) ([]SandboxRun, error) {
	if limit <= 0 {
		limit = 10
	}

	if s.db != nil {
		rows, err := s.db.QueryContext(ctx, `SELECT id, goal, command, exit_code, output, duration_seconds, is_noteworthy, insight, created_at
		FROM sandbox_runs ORDER BY created_at DESC LIMIT $1`, limit)
		if err == nil {
			defer rows.Close()
			var list []SandboxRun
			for rows.Next() {
				var r SandboxRun
				if err := rows.Scan(&r.ID, &r.Goal, &r.Command, &r.ExitCode, &r.Output, &r.DurationSeconds, &r.IsNoteworthy, &r.Insight, &r.CreatedAt); err == nil {
					list = append(list, r)
				}
			}
			if len(list) > 0 {
				return list, nil
			}
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []SandboxRun
	count := 0
	for i := len(s.memSandboxRuns) - 1; i >= 0 && count < limit; i-- {
		list = append(list, s.memSandboxRuns[i])
		count++
	}
	return list, nil
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
