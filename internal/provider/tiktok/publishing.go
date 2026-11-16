package tiktok

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

const (
	pollStage           = "tiktok_poll"
	postingStateVersion = 1
)

type creatorInfo struct {
	AvatarURL        string   `json:"creator_avatar_url,omitempty"`
	Username         string   `json:"creator_username"`
	Nickname         string   `json:"creator_nickname"`
	PrivacyLevels    []string `json:"privacy_level_options"`
	CommentDisabled  *bool    `json:"comment_disabled"`
	DuetDisabled     *bool    `json:"duet_disabled"`
	StitchDisabled   *bool    `json:"stitch_disabled"`
	MaxVideoDuration *int     `json:"max_video_post_duration_sec"`
}

func (c *Client) Capabilities(ctx context.Context, token model.Token) (model.Capabilities, error) {
	if err := requireScope(token, "video.publish"); err != nil {
		return model.Capabilities{}, err
	}
	res, err := c.request(ctx, http.MethodPost, "/v2/post/publish/creator_info/query/", token.AccessToken, "application/json; charset=UTF-8", []byte("{}"))
	if err != nil {
		return model.Capabilities{}, err
	}
	var info creatorInfo
	if err := decodeAPI(res, &info); err != nil {
		return model.Capabilities{}, err
	}
	if len(info.PrivacyLevels) == 0 || info.Nickname == "" || info.CommentDisabled == nil || info.DuetDisabled == nil || info.StitchDisabled == nil || info.MaxVideoDuration == nil || *info.MaxVideoDuration < 0 {
		return model.Capabilities{}, &provider.Error{Code: "invalid_response", Message: "TikTok did not return complete creator settings", Retryable: true}
	}
	accountType := "unknown"
	if contains(info.PrivacyLevels, "FOLLOWER_OF_CREATOR") && !contains(info.PrivacyLevels, "PUBLIC_TO_EVERYONE") {
		accountType = "private"
	}
	if contains(info.PrivacyLevels, "PUBLIC_TO_EVERYONE") && !contains(info.PrivacyLevels, "FOLLOWER_OF_CREATOR") {
		accountType = "public"
	}
	raw, _ := json.Marshal(info)
	return model.Capabilities{
		CanPublish: true, DisplayName: info.Nickname, AccountType: accountType,
		MaxVideoDurationSeconds: *info.MaxVideoDuration, PrivacyLevels: info.PrivacyLevels,
		CommentDisabled: *info.CommentDisabled, DuetDisabled: *info.DuetDisabled,
		StitchDisabled: *info.StitchDisabled, Raw: raw,
	}, nil
}

type videoPostInfo struct {
	Title          string `json:"title,omitempty"`
	PrivacyLevel   string `json:"privacy_level"`
	DisableComment bool   `json:"disable_comment"`
	DisableDuet    bool   `json:"disable_duet"`
	DisableStitch  bool   `json:"disable_stitch"`
	BrandContent   bool   `json:"brand_content_toggle"`
	BrandOrganic   bool   `json:"brand_organic_toggle"`
	IsAIGC         *bool  `json:"is_aigc,omitempty"`
	CoverTimestamp *int64 `json:"video_cover_timestamp_ms,omitempty"`
}

type photoPostInfo struct {
	Title          *string `json:"title,omitempty"`
	Description    string  `json:"description,omitempty"`
	PrivacyLevel   string  `json:"privacy_level"`
	DisableComment bool    `json:"disable_comment"`
	BrandContent   bool    `json:"brand_content_toggle"`
	BrandOrganic   bool    `json:"brand_organic_toggle"`
	AutoAddMusic   *bool   `json:"auto_add_music,omitempty"`
}

type postingState struct {
	Version       int      `json:"version"`
	OwnerID       string   `json:"owner_id"`
	ContentHash   string   `json:"content_hash"`
	PublishID     string   `json:"publish_id,omitempty"`
	PublicPostIDs []string `json:"public_post_ids,omitempty"`
}

func (c *Client) Publish(ctx context.Context, token model.Token, content model.Content, progress model.Progress, checkpoint provider.Checkpoint) (model.Outcome, error) {
	if err := ctx.Err(); err != nil {
		return model.Outcome{}, err
	}
	hasProgress := progress.Unsafe || progress.Stage != "" || len(progress.Data) != 0
	state, err := newPostingState(token, content)
	if err != nil {
		if hasProgress {
			return model.Outcome{}, uncertain(ctx, "invalid_progress", "Saved TikTok publishing progress cannot be matched to this account and content")
		}
		return model.Outcome{}, err
	}
	if hasProgress {
		var saved postingState
		if json.Unmarshal(progress.Data, &saved) != nil ||
			saved.Version != postingStateVersion ||
			saved.OwnerID != state.OwnerID ||
			saved.ContentHash != state.ContentHash {
			return model.Outcome{}, uncertain(ctx, "invalid_progress", "Saved TikTok publishing progress does not match this account, content, or state version")
		}
		if progress.Unsafe {
			return model.Outcome{}, uncertain(ctx, "publish_outcome_uncertain", "The previous TikTok initialization must be reconciled before another post can be sent")
		}
		if progress.Stage != pollStage || !validPublishID(saved.PublishID) {
			return model.Outcome{}, uncertain(ctx, "invalid_progress", "Saved TikTok publishing progress must be reconciled before continuing")
		}
		return c.poll(ctx, token, content, progress, saved)
	}
	if checkpoint == nil {
		return model.Outcome{}, localError("checkpoint_required", "TikTok publication requires durable checkpoint storage")
	}
	caps, err := c.Capabilities(ctx, token)
	if err != nil {
		return model.Outcome{}, err
	}
	if err := c.Validate(content, caps); err != nil {
		return model.Outcome{}, err
	}
	options, err := decodeOptions(content.Options)
	if err != nil {
		return model.Outcome{}, err
	}
	path, body, err := initializationRequest(content, options)
	if err != nil {
		return model.Outcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Outcome{}, err
	}
	intent := savedProgress(state)
	intent.Stage, intent.Unsafe = "tiktok_init", true
	if err := checkpoint(intent); err != nil {
		return model.Outcome{}, fmt.Errorf("save TikTok initialization checkpoint: %w", err)
	}
	res, requestErr := c.request(ctx, http.MethodPost, path, token.AccessToken, "application/json; charset=UTF-8", body)
	var data struct {
		PublishID string `json:"publish_id"`
	}
	apiErr := requestErr
	if apiErr == nil {
		apiErr = decodeAPI(res, &data)
	}
	if apiErr != nil {
		if explicitRejection(res) {
			// Definite rejection is the only condition that permits resetting
			// an initialized intent before a retry or authorization refresh.
			if err := checkpoint(model.Progress{}); err != nil {
				return model.Outcome{}, uncertain(ctx, "checkpoint_uncertain", "TikTok rejection could not be durably recorded; reconcile the saved attempt")
			}
			if requestErr != nil {
				apiErr = apiRejection("", res.status, res.retryAfter)
			}
			return model.Outcome{}, apiErr
		}
		return model.Outcome{}, uncertain(ctx, "publish_outcome_uncertain", "TikTok initialization could not be confirmed; reconcile the attempt before sending another post")
	}
	if !validPublishID(data.PublishID) {
		return model.Outcome{}, uncertain(ctx, "publish_outcome_uncertain", "TikTok accepted an initialization without a usable operation identifier; reconcile the attempt")
	}
	state.PublishID = data.PublishID
	next := savedProgress(state)
	if err := checkpoint(next); err != nil {
		return model.Outcome{}, uncertain(ctx, "checkpoint_uncertain", "TikTok accepted this post but its operation identifier could not be durably saved")
	}
	return model.Outcome{Progress: next, PollAfter: pollInterval, Warnings: mediaWarnings(content)}, nil
}

func initializationRequest(content model.Content, options Options) (string, []byte, error) {
	var body any
	path := "/v2/post/publish/video/init/"
	if isVideo(content.Media[0]) {
		body = struct {
			PostInfo   videoPostInfo `json:"post_info"`
			SourceInfo struct {
				Source   string `json:"source"`
				VideoURL string `json:"video_url"`
			} `json:"source_info"`
		}{
			PostInfo: videoPostInfo{
				Title: content.Text, PrivacyLevel: options.PrivacyLevel,
				DisableComment: !trueValue(options.AllowComment), DisableDuet: !trueValue(options.AllowDuet), DisableStitch: !trueValue(options.AllowStitch),
				BrandContent: trueValue(options.BrandContentToggle), BrandOrganic: trueValue(options.BrandOrganicToggle),
				IsAIGC: options.IsAIGC, CoverTimestamp: options.VideoCoverTimestampMS,
			},
			SourceInfo: struct {
				Source   string `json:"source"`
				VideoURL string `json:"video_url"`
			}{Source: "PULL_FROM_URL", VideoURL: content.Media[0].URL},
		}
	} else {
		path = "/v2/post/publish/content/init/"
		images := make([]string, len(content.Media))
		for index := range content.Media {
			images[index] = content.Media[index].URL
		}
		cover := 0
		if options.PhotoCoverIndex != nil {
			cover = *options.PhotoCoverIndex
		}
		body = struct {
			MediaType  string        `json:"media_type"`
			PostMode   string        `json:"post_mode"`
			IsAIGC     *bool         `json:"is_aigc,omitempty"`
			PostInfo   photoPostInfo `json:"post_info"`
			SourceInfo struct {
				Source          string   `json:"source"`
				PhotoImages     []string `json:"photo_images"`
				PhotoCoverIndex int      `json:"photo_cover_index"`
			} `json:"source_info"`
		}{
			MediaType: "PHOTO", PostMode: "DIRECT_POST", IsAIGC: options.IsAIGC,
			PostInfo: photoPostInfo{
				Title: options.Title, Description: content.Text, PrivacyLevel: options.PrivacyLevel,
				DisableComment: !trueValue(options.AllowComment),
				BrandContent:   trueValue(options.BrandContentToggle), BrandOrganic: trueValue(options.BrandOrganicToggle),
				AutoAddMusic: options.AutoAddMusic,
			},
			SourceInfo: struct {
				Source          string   `json:"source"`
				PhotoImages     []string `json:"photo_images"`
				PhotoCoverIndex int      `json:"photo_cover_index"`
			}{Source: "PULL_FROM_URL", PhotoImages: images, PhotoCoverIndex: cover},
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", nil, localError("invalid_content", "TikTok posting content could not be encoded")
	}
	return path, encoded, nil
}

func (c *Client) poll(ctx context.Context, token model.Token, content model.Content, progress model.Progress, saved postingState) (model.Outcome, error) {
	if err := requireScope(token, "video.publish"); err != nil {
		return model.Outcome{}, err
	}
	// This limit is per account, across every pending operation and replica.
	// Worker-installed pacing returns a durable wait instead of sleeping here.
	const statusSpacing = 2100 * time.Millisecond
	if err := provider.BeforeRequest(ctx, "tiktok.status", statusSpacing); err != nil {
		return model.Outcome{}, err
	}
	body, _ := json.Marshal(struct {
		PublishID string `json:"publish_id"`
	}{saved.PublishID})
	res, err := c.request(ctx, http.MethodPost, "/v2/post/publish/status/fetch/", token.AccessToken, "application/json; charset=UTF-8", body)
	var status struct {
		Status        string   `json:"status"`
		FailReason    string   `json:"fail_reason"`
		PublicPostIDs []postID `json:"publicaly_available_post_id"`
	}
	if err == nil {
		err = decodeAPI(res, &status)
	}
	delay := statusSpacing
	var limited *provider.Error
	if errors.As(err, &limited) && limited.RateLimited && provider.RateLimitDelay(limited) > delay {
		delay = provider.RateLimitDelay(limited)
	}
	if pacingErr := provider.AfterRequest(ctx, "tiktok.status", delay); pacingErr != nil {
		return model.Outcome{}, pacingErr
	}
	if err != nil {
		return model.Outcome{}, err
	}
	warnings := mediaWarnings(content)
	switch status.Status {
	case "PROCESSING_UPLOAD", "PROCESSING_DOWNLOAD":
		return model.Outcome{Progress: progress, PollAfter: pollInterval, Warnings: warnings}, nil
	case "PUBLISH_COMPLETE":
		id := saved.PublishID
		saved.PublicPostIDs = nil
		for _, publicID := range status.PublicPostIDs {
			saved.PublicPostIDs = append(saved.PublicPostIDs, string(publicID))
		}
		if len(saved.PublicPostIDs) > 0 {
			id = saved.PublicPostIDs[0]
			if len(saved.PublicPostIDs) > 1 {
				warnings = append(warnings, "TikTok returned multiple public post IDs; the receipt identifies the first and saved progress retains all of them.")
			}
		} else {
			warnings = append(warnings, "TikTok completed the post without a public post ID. The receipt contains the publish operation ID; this can occur for private or not-yet-moderated posts.")
		}
		return model.Outcome{Done: true, RemoteID: id, Progress: savedProgress(saved), Warnings: warnings}, nil
	case "FAILED":
		// A terminal provider failure ends this attempt. Even for a provider
		// failure that permits a new attempt, a user must submit that new
		// attempt explicitly; polling never silently creates another post.
		return model.Outcome{}, terminalFailure(status.FailReason)
	case "SEND_TO_USER_INBOX":
		return model.Outcome{}, localError("unexpected_draft", "TikTok delivered this content as a draft that requires completion in the TikTok app")
	default:
		return model.Outcome{}, &provider.Error{Code: "unknown_post_status", Message: "TikTok returned an unrecognized processing state; the saved operation will be checked again", Retryable: true}
	}
}

func savedProgress(data postingState) model.Progress {
	encoded, _ := json.Marshal(data)
	return model.Progress{Stage: pollStage, Data: encoded}
}

func newPostingState(token model.Token, content model.Content) (postingState, error) {
	if token.ProviderUserID == "" {
		return postingState{}, localError("identity_required", "TikTok publishing requires a verified connection identity")
	}
	if _, err := decodeOptions(content.Options); err != nil {
		return postingState{}, err
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return postingState{}, localError("invalid_content", "TikTok publishing content could not be bound to its checkpoint")
	}
	// Normalize nested RawMessage object ordering and whitespace as well as the
	// outer model. UseNumber avoids converting JSON integers through float64.
	var value any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return postingState{}, localError("invalid_content", "TikTok publishing content could not be bound to its checkpoint")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return postingState{}, localError("invalid_content", "TikTok publishing content could not be bound to its checkpoint")
	}
	digest := sha256.Sum256(canonical)
	return postingState{
		Version:     postingStateVersion,
		OwnerID:     token.ProviderUserID,
		ContentHash: fmt.Sprintf("%x", digest),
	}, nil
}

func validPublishID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// A custom decoder retains all digits whether TikTok sends an integer or a
// quoted integer. Decoding through float64 would corrupt many TikTok IDs.
type postID string

func (id *postID) UnmarshalJSON(raw []byte) error {
	value := string(bytes.TrimSpace(raw))
	if strings.HasPrefix(value, "\"") {
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("invalid TikTok post identifier")
		}
	}
	if value == "" || len(value) > 64 {
		return fmt.Errorf("invalid TikTok post identifier")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return fmt.Errorf("invalid TikTok post identifier")
		}
	}
	*id = postID(value)
	return nil
}

func terminalFailure(reason string) error {
	result := localError("publish_failed", "TikTok could not publish this content; review it before submitting another attempt")
	switch reason {
	case "file_format_check_failed", "duration_check_failed", "frame_rate_check_failed", "picture_size_check_failed":
		result.Code, result.Message = reason, "TikTok rejected the media format or dimensions; inspect the original media before trying again"
	case "video_pull_failed", "photo_pull_failed":
		result.Code, result.Message = reason, "TikTok could not download the media; verify its availability before submitting another attempt"
	case "internal":
		result.Code, result.Message = reason, "TikTok processing failed; a new creator-approved attempt may be submitted later"
	case "auth_removed":
		result.Code, result.Message = reason, "TikTok authorization was removed and this publishing attempt has ended"
	case "spam_risk_too_many_posts", "spam_risk_user_banned_from_posting", "spam_risk_text", "spam_risk":
		result.Code, result.Message = reason, "TikTok stopped this publishing attempt because of an account or content restriction"
	case "publish_cancelled":
		result.Code, result.Message = reason, "TikTok cancelled the media download"
	}
	return result
}

func mediaWarnings(content model.Content) []string {
	for _, media := range content.Media {
		if media.AltText != "" {
			return []string{"TikTok's Direct Post API does not accept this media's alternate text; it was not sent to TikTok."}
		}
	}
	return nil
}
