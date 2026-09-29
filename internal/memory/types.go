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

// MoltbookMemory represents something Shipp read and found worth remembering from the Moltbook AI social network.
type MoltbookMemory struct {
	ID          int64     `json:"id"`
	PostID      string    `json:"post_id"`
	PostTitle   string    `json:"post_title"`
	Author      string    `json:"author"`
	Content     string    `json:"content"`     // extracted key insight / summary (not full post)
	Tags        string    `json:"tags"`        // comma-separated topics e.g. "security,stateless,agents"
	Upvotes     int       `json:"upvotes"`
	InfluencedAction string `json:"influenced_action"` // what shipp did as a result, if anything
	SavedAt     time.Time `json:"saved_at"`
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
	FindMessagesBySender(ctx context.Context, username string, limit int) ([]Message, error)
	// Moltbook long-term memory - things Shipp read and chose to remember
	SaveMoltbookMemory(ctx context.Context, mem MoltbookMemory) error
	GetMoltbookMemories(ctx context.Context, limit int) ([]MoltbookMemory, error)
	SearchMoltbookMemories(ctx context.Context, query string, limit int) ([]MoltbookMemory, error)
	// Living Identity (Tier 2 self-tuning narrative authored and updated by Shipp)
	GetSelfIdentity(ctx context.Context, key string) (string, error)
	GetAllSelfIdentity(ctx context.Context) (map[string]string, error)
	SaveSelfIdentity(ctx context.Context, key string, content string) error
	// Autonomous Sandbox Execution Memory
	SaveSandboxRun(ctx context.Context, run SandboxRun) error
	GetRecentSandboxRuns(ctx context.Context, limit int) ([]SandboxRun, error)
	GetDB() *sql.DB
	GetRedis() *redis.Client
	Close() error
}

type AgentSelfIdentity struct {
	Key       string    `json:"key"`
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SandboxRun struct {
	ID              int64     `json:"id"`
	Goal            string    `json:"goal"`
	Command         string    `json:"command"`
	ExitCode        int       `json:"exit_code"`
	Output          string    `json:"output"`
	DurationSeconds int       `json:"duration_seconds"`
	IsNoteworthy    bool      `json:"is_noteworthy"`
	Insight         string    `json:"insight"`
	CreatedAt       time.Time `json:"created_at"`
}
