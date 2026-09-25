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

type Store interface {
	SaveMessage(ctx context.Context, chatID int64, senderID int64, username, role, content string) error
	GetRecentMessages(ctx context.Context, chatID int64, limit int) ([]Message, error)
	ClearContext(ctx context.Context, chatID int64) error
	SaveSummary(ctx context.Context, chatID int64, summary string) error
	GetSummary(ctx context.Context, chatID int64) (string, error)
	GetActiveChatIDs(ctx context.Context) ([]int64, error)
	GetDB() *sql.DB
	GetRedis() *redis.Client
	Close() error
}
