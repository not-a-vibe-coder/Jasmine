package memory

import (
	"context"
	"database/sql"
	"time"

	"github.com/redis/go-redis/v9"
)

type Message struct {
	Role      string    `json:"role"`
	Sender    string    `json:"sender"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type UserProfile struct {
	ChatID         int64     `json:"chat_id"`
	Preferences    string    `json:"preferences"`
	ActiveProjects string    `json:"active_projects"`
	LifeContext    string    `json:"life_context"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Store interface {
	SaveMessage(ctx context.Context, chatID int64, senderID int64, username, role, content string) error
	GetRecentMessages(ctx context.Context, chatID int64, limit int) ([]Message, error)
	ClearContext(ctx context.Context, chatID int64) error
	SaveSummary(ctx context.Context, chatID int64, summary string) error
	GetSummary(ctx context.Context, chatID int64) (string, error)
	SaveUserProfile(ctx context.Context, profile UserProfile) error
	GetUserProfile(ctx context.Context, chatID int64) (*UserProfile, error)
	GetActiveChatIDs(ctx context.Context) ([]int64, error)
	GetUserIDByUsername(ctx context.Context, username string) (int64, error)
	GetDB() *sql.DB
	GetRedis() *redis.Client
	Close() error
}
