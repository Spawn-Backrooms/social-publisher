package model

import (
	"encoding/json"
	"time"
)

type Platform string

const (
	TikTok    Platform = "tiktok"
	X         Platform = "x"
	Instagram Platform = "instagram"
)

type Principal struct {
	TenantID string `json:"tenant_id"`
	Subject  string `json:"sub"`
}
type Media struct {
	URL             string  `json:"url"`
	MIMEType        string  `json:"mime_type"`
	SizeBytes       int64   `json:"size_bytes"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Width           int     `json:"width,omitempty"`
	Height          int     `json:"height,omitempty"`
	AltText         string  `json:"alt_text,omitempty"`
}
type Content struct {
	Text    string          `json:"text"`
	Media   []Media         `json:"media,omitempty"`
	Options json.RawMessage `json:"options,omitempty"`
	// Reply is server-resolved from a confirmed local publisher receipt.
	Reply         *XReply    `json:"reply,omitempty"`
	PublishBefore *time.Time `json:"publish_before,omitempty"`
}
type XReply struct {
	PostID   string `json:"post_id"`
	RemoteID string `json:"remote_id"`
	OwnerID  string `json:"owner_id"`
}
type Token struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	TokenType        string    `json:"token_type,omitempty"`
	Scopes           []string  `json:"scopes,omitempty"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitempty"`
	ProviderUserID   string    `json:"provider_user_id,omitempty"`
}
type Identity struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	AccountType string `json:"account_type,omitempty"`
}
type Capabilities struct {
	CanPublish              bool            `json:"can_publish"`
	DisplayName             string          `json:"display_name,omitempty"`
	AccountType             string          `json:"account_type,omitempty"`
	MaxVideoDurationSeconds int             `json:"max_video_duration_seconds,omitempty"`
	PrivacyLevels           []string        `json:"privacy_levels,omitempty"`
	CommentDisabled         bool            `json:"comment_disabled,omitempty"`
	DuetDisabled            bool            `json:"duet_disabled,omitempty"`
	StitchDisabled          bool            `json:"stitch_disabled,omitempty"`
	Raw                     json.RawMessage `json:"raw,omitempty"`
}
type Progress struct {
	Stage  string          `json:"stage,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Unsafe bool            `json:"unsafe,omitempty"`
}
type Outcome struct {
	Done      bool
	RemoteID  string
	RemoteURL string
	Progress  Progress
	PollAfter time.Duration
	Warnings  []string
}

type Account struct {
	ID                     string    `json:"id"`
	Owner                  Principal `json:"-"`
	Platform               Platform  `json:"platform"`
	Identity               Identity  `json:"identity"`
	Status                 string    `json:"status"`
	EncryptedToken         []byte    `json:"-"`
	TokenVersion           int64     `json:"-"`
	TokenRefreshVersion    int64     `json:"-"`
	TokenRefreshStartedAt  time.Time `json:"-"`
	TokenExpiresAt         time.Time `json:"token_expires_at"`
	AuthorizationStartedAt time.Time `json:"-"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

func (a Account) TokenAAD() string {
	return a.Owner.TenantID + "\x00" + a.Owner.Subject + "\x00" + string(a.Platform) + "\x00" + a.Identity.ID
}

type TargetRequest struct {
	AccountID     string          `json:"account_id"`
	Text          *string         `json:"text,omitempty"`
	Media         *[]Media        `json:"media,omitempty"`
	Options       json.RawMessage `json:"options,omitempty"`
	ReplyToPostID string          `json:"reply_to_post_id,omitempty"`
	PublishBefore *time.Time      `json:"publish_before,omitempty"`
}
type CreatePostRequest struct {
	Text      string          `json:"text"`
	Media     []Media         `json:"media,omitempty"`
	Targets   []TargetRequest `json:"targets"`
	PublishAt *time.Time      `json:"publish_at,omitempty"`
	Timezone  string          `json:"timezone,omitempty"`
}
type Post struct {
	ID        string    `json:"id"`
	Owner     Principal `json:"-"`
	Status    string    `json:"status"`
	PublishAt time.Time `json:"publish_at"`
	Timezone  string    `json:"timezone,omitempty"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Targets   []Target  `json:"targets"`
}
type Target struct {
	ID                  string     `json:"id"`
	PostID              string     `json:"post_id"`
	AccountID           string     `json:"account_id"`
	Platform            Platform   `json:"platform"`
	Content             Content    `json:"content"`
	Status              string     `json:"status"`
	Progress            Progress   `json:"-"`
	Attempts            int        `json:"attempts"`
	Failures            int        `json:"failures"`
	NextAttemptAt       time.Time  `json:"next_attempt_at"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	ProcessingStartedAt *time.Time `json:"processing_started_at,omitempty"`
	RemoteID            string     `json:"remote_id,omitempty"`
	RemoteURL           string     `json:"remote_url,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	Warnings            []string   `json:"warnings,omitempty"`
	LeaseToken          string     `json:"-"`
	LeaseUntil          *time.Time `json:"-"`
	ClaimedAt           time.Time  `json:"-"`
}
type OAuthState struct {
	StateHash         string
	CookieHash        string
	Owner             Principal
	Platform          Platform
	EncryptedVerifier []byte
	IssuedAt          time.Time
	ExpiresAt         time.Time
}
type Event struct {
	ID        int64     `json:"id"`
	TargetID  string    `json:"target_id,omitempty"`
	Kind      string    `json:"kind"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
