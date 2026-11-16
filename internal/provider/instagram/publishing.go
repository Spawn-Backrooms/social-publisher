package instagram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

const (
	stageContainers = "instagram:containers"
	stageProcessing = "instagram:processing"
	stagePublishing = "instagram:publishing"
	stagePublished  = "instagram:published"
	pollInterval    = time.Minute
)

type publishState struct {
	Version     int      `json:"version"`
	AccountID   string   `json:"account_id"`
	Fingerprint string   `json:"fingerprint"`
	Children    []string `json:"children,omitempty"`
	ContainerID string   `json:"container_id,omitempty"`
	PublishedID string   `json:"published_id,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Renewals    int      `json:"renewals,omitempty"`
}

func (c *Client) Publish(ctx context.Context, token model.Token, content model.Content, progress model.Progress, checkpoint provider.Checkpoint) (model.Outcome, error) {
	// Neither a retry nor a FINISHED status proves that an earlier publication
	// failed. Unsafe progress must be reconciled explicitly by the application.
	if progress.Unsafe || progress.Stage == stagePublishing {
		return model.Outcome{Progress: progress}, uncertainPublication()
	}
	if err := c.Validate(content, model.Capabilities{CanPublish: true, MaxVideoDurationSeconds: 900}); err != nil {
		return model.Outcome{Progress: progress}, err
	}
	if checkpoint == nil {
		return model.Outcome{Progress: progress}, localError("instagram_checkpoint_required", "Durable progress storage is required before Instagram publishing.")
	}
	state, err := readState(content, progress)
	if err != nil {
		return model.Outcome{Progress: progress}, err
	}
	if len(token.Scopes) > 0 && (!contains(token.Scopes, publishingScopes[0]) || !contains(token.Scopes, publishingScopes[1])) {
		return model.Outcome{Progress: progress}, localError("instagram_permission_denied", "The Instagram connection requires basic and content-publishing permissions.")
	}
	accountID := token.ProviderUserID
	if accountID == "" {
		identity, err := c.Identity(ctx, token)
		if err != nil {
			return model.Outcome{Progress: progress}, err
		}
		accountID = identity.ID
	}
	if !validID(accountID) {
		return model.Outcome{Progress: progress}, localError("invalid_account_id", "The connected Instagram account ID is invalid.")
	}
	if state.AccountID != "" && state.AccountID != accountID {
		return model.Outcome{Progress: progress}, localError("instagram_progress_mismatch", "Instagram progress belongs to a different account.")
	}
	state.AccountID = accountID
	if state.PublishedID != "" {
		return model.Outcome{Done: true, RemoteID: state.PublishedID, Progress: progress, Warnings: append([]string(nil), state.Warnings...)}, nil
	}
	current := progress
	save := func(stage string, unsafe bool) error {
		data, err := json.Marshal(state)
		if err != nil {
			return localError("instagram_checkpoint_invalid", "Instagram progress could not be encoded.")
		}
		next := model.Progress{Stage: stage, Data: data, Unsafe: unsafe}
		if err := checkpoint(next); err != nil {
			return err
		}
		current = next
		return nil
	}
	failure := func(err error) (model.Outcome, error) { return model.Outcome{Progress: current}, err }
	waiting := func(after time.Duration) (model.Outcome, error) {
		return model.Outcome{Progress: current, PollAfter: after}, nil
	}
	if state.ContainerID == "" && noChildren(state.Children) {
		if err := c.checkQuota(ctx, token, accountID); err != nil {
			return failure(err)
		}
	}
	opt, _ := parseOptions(content.Options) // Validate already decoded and checked options.
	if state.ContainerID == "" {
		if len(content.Media) == 1 {
			params := mediaParams(content.Media[0], false)
			params.Set("caption", content.Text)
			if inferredType(content) == "REELS" {
				params.Set("media_type", "REELS")
				share := true
				if opt.ShareToFeed != nil {
					share = *opt.ShareToFeed
				}
				params.Set("share_to_feed", strconv.FormatBool(share))
			}
			id, err := c.createContainer(ctx, token, accountID, params)
			if err != nil {
				return failure(err)
			}
			state.ContainerID = id
			if err := save(stageProcessing, false); err != nil {
				return failure(err)
			}
		} else {
			for i, media := range content.Media {
				if state.Children[i] != "" {
					continue
				}
				id, err := c.createContainer(ctx, token, accountID, mediaParams(media, true))
				if err != nil {
					return failure(err)
				}
				state.Children[i] = id
				if err := save(stageContainers, false); err != nil {
					return failure(err)
				}
			}
			ready := true
			for _, child := range state.Children {
				status, err := c.containerStatus(ctx, token, child)
				if err != nil {
					return failure(err)
				}
				switch status {
				case "FINISHED":
				case "IN_PROGRESS":
					ready = false
				case "EXPIRED":
					if state.Renewals >= 2 {
						return failure(localError("instagram_container_expired", "Instagram media containers repeatedly expired before publication; create a new post when ready."))
					}
					state.Renewals++
					// Renew the set together so its children have comparable
					// lifetimes when a delayed carousel finally gets published.
					for i := range state.Children {
						state.Children[i] = ""
					}
					if err := save(stageContainers, false); err != nil {
						return failure(err)
					}
					return waiting(time.Second)
				case "PUBLISHED":
					return failure(uncertainPublication())
				default:
					return failure(statusError(status))
				}
			}
			if !ready {
				return waiting(pollInterval)
			}
			id, err := c.createContainer(ctx, token, accountID, url.Values{
				"media_type": {"CAROUSEL"}, "children": {strings.Join(state.Children, ",")}, "caption": {content.Text},
			})
			if err != nil {
				return failure(err)
			}
			state.ContainerID = id
			if err := save(stageProcessing, false); err != nil {
				return failure(err)
			}
		}
	}
	status, err := c.containerStatus(ctx, token, state.ContainerID)
	if err != nil {
		return failure(err)
	}
	switch status {
	case "IN_PROGRESS":
		return waiting(pollInterval)
	case "EXPIRED":
		if state.Renewals >= 2 {
			return failure(localError("instagram_container_expired", "Instagram media containers repeatedly expired before publication; create a new post when ready."))
		}
		state.Renewals++
		state.ContainerID = ""
		for i := range state.Children {
			state.Children[i] = ""
		}
		if err := save(stageContainers, false); err != nil {
			return failure(err)
		}
		return waiting(time.Second)
	case "PUBLISHED":
		// Container IDs are not published-media IDs. Do not fabricate a receipt.
		return failure(uncertainPublication())
	case "FINISHED":
	default:
		return failure(statusError(status))
	}
	if err := c.checkQuota(ctx, token, accountID); err != nil {
		return failure(err)
	}
	if err := ctx.Err(); err != nil {
		return failure(err)
	}
	if err := save(stagePublishing, true); err != nil {
		return failure(err)
	}
	body, err := c.request(ctx, http.MethodPost, c.graph(accountID+"/media_publish"), url.Values{
		"creation_id": {state.ContainerID},
	}, token.AccessToken, requestPublish)
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && !pe.Ambiguous {
			// A proven rejection can be retried only after removing the durable
			// unsafe marker. A failed checkpoint cannot establish that transition.
			if saveErr := save(stageProcessing, false); saveErr != nil {
				return failure(uncertainPublication())
			}
		}
		return failure(err)
	}
	var response struct {
		ID          graphID         `json:"id"`
		ConfigIssue json.RawMessage `json:"config_issue"`
	}
	if err := decodeJSON(body, &response); err != nil || response.ID == "" {
		return failure(uncertainPublication())
	}
	state.PublishedID = string(response.ID)
	state.Warnings = configurationWarnings(response.ConfigIssue)
	if err := save(stagePublished, false); err != nil {
		return failure(uncertainPublication())
	}
	return model.Outcome{Done: true, RemoteID: state.PublishedID, Progress: current, Warnings: append([]string(nil), state.Warnings...)}, nil
}

func readState(content model.Content, progress model.Progress) (publishState, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return publishState{}, localError("instagram_invalid_content", "Instagram content could not be encoded.")
	}
	hash := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(hash[:])
	if progress.Stage == "" && len(progress.Data) == 0 && !progress.Unsafe {
		state := publishState{Version: 1, Fingerprint: fingerprint}
		if len(content.Media) > 1 {
			state.Children = make([]string, len(content.Media))
		}
		return state, nil
	}
	invalid := func() (publishState, error) {
		return publishState{}, localError("instagram_invalid_progress", "Instagram progress is invalid; reconcile the target before restarting it.")
	}
	if len(progress.Data) == 0 || len(progress.Data) > 64*1024 {
		return invalid()
	}
	var state publishState
	d := json.NewDecoder(bytes.NewReader(progress.Data))
	d.DisallowUnknownFields()
	if err := d.Decode(&state); err != nil {
		return invalid()
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF || state.Version != 1 || !validID(state.AccountID) || state.Fingerprint != fingerprint || state.Renewals < 0 || state.Renewals > 2 {
		return invalid()
	}
	if (len(content.Media) > 1 && len(state.Children) != len(content.Media)) || (len(content.Media) == 1 && len(state.Children) != 0) {
		return invalid()
	}
	for _, id := range state.Children {
		if id != "" && !validID(id) {
			return invalid()
		}
	}
	if (state.ContainerID != "" && !validID(state.ContainerID)) || (state.PublishedID != "" && !validID(state.PublishedID)) {
		return invalid()
	}
	switch progress.Stage {
	case stageContainers:
		if state.ContainerID != "" || state.PublishedID != "" {
			return invalid()
		}
	case stageProcessing:
		if state.ContainerID == "" || state.PublishedID != "" {
			return invalid()
		}
	case stagePublished:
		if state.ContainerID == "" || state.PublishedID == "" {
			return invalid()
		}
	default:
		return invalid()
	}
	if state.ContainerID != "" && len(content.Media) > 1 {
		for _, id := range state.Children {
			if id == "" {
				return invalid()
			}
		}
	}
	// Stored warnings are generated locally. Do not forward modified state text.
	for _, warning := range state.Warnings {
		if !contains(allConfigurationWarnings(), warning) {
			return invalid()
		}
	}
	return state, nil
}

func mediaParams(media model.Media, child bool) url.Values {
	params := url.Values{}
	if mediaMIME(media.MIMEType) == "image/jpeg" {
		params.Set("image_url", media.URL)
		if media.AltText != "" {
			params.Set("alt_text", media.AltText)
		}
	} else {
		params.Set("video_url", media.URL)
		if child {
			params.Set("media_type", "VIDEO")
		}
	}
	if child {
		params.Set("is_carousel_item", "true")
	}
	return params
}

func (c *Client) createContainer(ctx context.Context, token model.Token, accountID string, params url.Values) (string, error) {
	body, err := c.request(ctx, http.MethodPost, c.graph(accountID+"/media"), params, token.AccessToken, requestCreate)
	if err != nil {
		return "", err
	}
	var response struct {
		ID graphID `json:"id"`
	}
	if err := decodeJSON(body, &response); err != nil || response.ID == "" {
		// This operation cannot publish. A retry may leave an orphan container,
		// but must never fabricate a missing ID or skip its checkpoint.
		return "", &provider.Error{Code: "instagram_container_response_invalid", Message: "Instagram did not return a usable media container ID.", Retryable: true}
	}
	return string(response.ID), nil
}

func (c *Client) containerStatus(ctx context.Context, token model.Token, id string) (string, error) {
	body, err := c.request(ctx, http.MethodGet, c.graph(id), url.Values{"fields": {"status_code,status"}}, token.AccessToken, requestRead)
	if err != nil {
		return "", err
	}
	var response struct {
		StatusCode string `json:"status_code"`
	}
	if err := decodeJSON(body, &response); err != nil || response.StatusCode == "" {
		return "", malformedResponse("Instagram did not return a media container status.")
	}
	return response.StatusCode, nil
}

func (c *Client) checkQuota(ctx context.Context, token model.Token, accountID string) error {
	quota, err := c.quota(ctx, token, accountID)
	if err != nil {
		return err
	}
	if quota.Usage >= quota.Total {
		return &provider.Error{Code: "instagram_quota_exhausted", Message: "The Instagram account's current publishing quota is exhausted.", Retryable: true, RateLimited: true,
			RetryAfter: min(15*time.Minute, time.Duration(quota.DurationSeconds)*time.Second)}
	}
	return nil
}

func noChildren(ids []string) bool {
	for _, id := range ids {
		if id != "" {
			return false
		}
	}
	return true
}

func statusError(status string) error {
	if status == "ERROR" {
		return localError("instagram_container_failed", "Instagram could not process the media container; check the source media and create a corrected post.")
	}
	return localError("instagram_container_status_unknown", "Instagram returned an unsupported media container status.")
}

func uncertainPublication() *provider.Error {
	return &provider.Error{Code: "instagram_publication_uncertain", Message: "Instagram publication may already have succeeded; reconcile this target before another publish attempt.", Ambiguous: true}
}

func allConfigurationWarnings() []string {
	return []string{
		"Instagram published the media without its caption; review the published content.",
		"Instagram published the media without its user tags; review the published content.",
		"Instagram reported a publication configuration issue; review the published content.",
	}
}

func configurationWarnings(raw json.RawMessage) []string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var issue string
	err := json.Unmarshal(raw, &issue)
	if err == nil && issue == "" {
		return nil
	}
	warnings := allConfigurationWarnings()
	switch issue {
	case "CAPTION_NOT_ATTACHED":
		return warnings[:1]
	case "USER_TAGGING_FAILURE":
		return warnings[1:2]
	default:
		return warnings[2:]
	}
}
