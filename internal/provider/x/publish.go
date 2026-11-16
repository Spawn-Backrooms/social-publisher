package x

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

const chunkSize = 4 * 1024 * 1024

// Only identifiers and digests are stored here, never tokens or source URLs.
// Versioning makes an incompatible stored checkpoint fail closed.
type publishState struct {
	Version     int          `json:"version"`
	ContentHash string       `json:"content_hash"`
	OwnerID     string       `json:"owner_id,omitempty"`
	Media       []mediaState `json:"media,omitempty"`
	PostID      string       `json:"post_id,omitempty"`
}

type mediaState struct {
	ID             string    `json:"id,omitempty"`
	Stage          string    `json:"stage,omitempty"`
	TotalBytes     int64     `json:"total_bytes,omitempty"`
	UploadedBytes  int64     `json:"uploaded_bytes,omitempty"`
	NextSegment    int       `json:"next_segment,omitempty"`
	PrefixSHA256   string    `json:"prefix_sha256,omitempty"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	NextPollAt     time.Time `json:"next_poll_at,omitempty"`
	AltTextApplied bool      `json:"alt_text_applied,omitempty"`
}

type mediaResult struct {
	ID               string          `json:"id"`
	ExpiresAfterSecs int64           `json:"expires_after_secs"`
	ExpiresAt        int64           `json:"expires_at"`
	Processing       *processingInfo `json:"processing_info"`
}

type processingInfo struct {
	State          string `json:"state"`
	CheckAfterSecs int64  `json:"check_after_secs"`
}

type mediaEnvelope struct {
	Data   *mediaResult `json:"data"`
	Errors []struct {
		Status int `json:"status"`
	} `json:"errors"`
}

func (c *Client) decodeMediaResponse(r response) (mediaEnvelope, error) {
	var wire mediaEnvelope
	if err := json.Unmarshal(r.body, &wire); err != nil {
		return wire, &provider.Error{Code: "x_invalid_media_response", Message: "X returned a malformed media response. No publication will be attempted.", Retryable: true}
	}
	if len(wire.Errors) > 0 {
		// Read only a structured status for classification, never provider text.
		// HTTP 200 can contain failed suboperations and is not an acknowledgement.
		if len(wire.Errors) == 1 && wire.Errors[0].Status >= 400 && wire.Errors[0].Status <= 599 {
			r.status = wire.Errors[0].Status
			return wire, c.responseError(r, false)
		}
		return wire, invalid("x_media_error", "X reported an error while preparing media. The post has not been published.")
	}
	if wire.Data == nil {
		return wire, &provider.Error{Code: "x_invalid_media_response", Message: "X did not confirm the media operation. No publication will be attempted.", Retryable: true}
	}
	return wire, nil
}

type publication struct {
	client     *Client
	ctx        context.Context
	token      model.Token
	content    model.Content
	data       publishState
	current    model.Progress
	checkpoint provider.Checkpoint
}

func (c *Client) Publish(ctx context.Context, token model.Token, content model.Content, progress model.Progress, checkpoint provider.Checkpoint) (model.Outcome, error) {
	var state publishState
	if len(progress.Data) > 0 {
		if err := json.Unmarshal(progress.Data, &state); err != nil {
			if progress.Unsafe {
				return model.Outcome{Progress: progress}, uncertain()
			}
			return model.Outcome{Progress: progress}, invalid("x_invalid_checkpoint", "The saved X upload checkpoint cannot be read.")
		}
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return model.Outcome{Progress: progress}, invalid("x_invalid_content", "The content cannot be serialized for X.")
	}
	digest := sha256.Sum256(encoded)
	contentHash := hex.EncodeToString(digest[:])
	// Even a successful receipt must belong to this exact target and account.
	// These checks require identity, not a fresh access token or a network call.
	if len(progress.Data) > 0 && (state.Version != 1 || state.ContentHash != contentHash || len(state.Media) != len(content.Media) || !validID(state.OwnerID) || state.OwnerID != token.ProviderUserID) {
		return model.Outcome{Progress: progress}, invalid("x_checkpoint_mismatch", "The saved X upload belongs to different content, an account, or an unsupported checkpoint version.")
	}
	// A bound authoritative receipt takes precedence over token expiry, later
	// validation changes, and a stale unsafe bit.
	if validID(state.PostID) {
		progress.Unsafe, progress.Stage = false, "x.published"
		return published(state.PostID, progress), nil
	}
	if progress.Unsafe {
		return model.Outcome{Progress: progress}, uncertain()
	}
	if deadline := content.PublishBefore; deadline != nil && !deadline.After(c.now()) {
		return model.Outcome{Progress: progress}, invalid("x_publication_window_expired", "The approved publication window has ended.")
	}
	if err := ctx.Err(); err != nil {
		return model.Outcome{Progress: progress}, err
	}
	if checkpoint == nil {
		return model.Outcome{Progress: progress}, invalid("x_checkpoint_required", "Durable checkpoints are required before X publication.")
	}
	if token.AccessToken == "" {
		return model.Outcome{Progress: progress}, &provider.Error{Code: "x_auth_expired", Message: "Connect the X account before publishing.", AuthExpired: true}
	}
	if !validID(token.ProviderUserID) {
		return model.Outcome{Progress: progress}, invalid("x_identity_required", "The connected X account identity is required before publishing.")
	}
	if content.Reply != nil && content.Reply.OwnerID != token.ProviderUserID {
		return model.Outcome{Progress: progress}, invalid("x_thread_owner_mismatch", "The thread belongs to a different X identity.")
	}
	if len(token.Scopes) > 0 && !hasScopes(token.Scopes, "tweet.read", "tweet.write", "users.read") {
		return model.Outcome{Progress: progress}, invalid("x_missing_scope", "Reconnect the X account with post creation permissions.")
	}
	if len(content.Media) > 0 && len(token.Scopes) > 0 && !hasScopes(token.Scopes, "media.write") {
		return model.Outcome{Progress: progress}, invalid("x_missing_media_scope", "Reconnect the X account with media.write permission.")
	}
	// The worker performs live account capability validation. This second
	// structural validation applies the platform's maximum bounds and prevents
	// unsupported request fields even when Publish is called directly.
	if err := c.Validate(content, model.Capabilities{CanPublish: true, MaxVideoDurationSeconds: 7500}); err != nil {
		return model.Outcome{Progress: progress}, err
	}
	if len(progress.Data) == 0 {
		state = publishState{Version: 1, ContentHash: contentHash, OwnerID: token.ProviderUserID, Media: make([]mediaState, len(content.Media))}
	}
	p := &publication{client: c, ctx: ctx, token: token, content: content, data: state, current: progress, checkpoint: checkpoint}
	for i := range content.Media {
		if err := p.validateMediaState(i); err != nil {
			return p.outcome(0), err
		}
		wait, err := p.prepareMedia(i)
		if err != nil || wait > 0 {
			return p.outcome(wait), err
		}
		if content.Media[i].AltText != "" && !p.data.Media[i].AltTextApplied {
			if err := p.applyAltText(i); err != nil {
				return p.outcome(0), err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return p.outcome(0), err
	}
	if err := p.save("x.ready", false); err != nil {
		return p.outcome(0), err
	}
	return p.createPost()
}

func (p *publication) validateMediaState(index int) error {
	m := p.data.Media[index]
	if m.ID == "" {
		if m.Stage != "" || m.UploadedBytes != 0 || m.NextSegment != 0 {
			return invalid("x_invalid_checkpoint", "An X media checkpoint is incomplete.")
		}
		return nil
	}
	if !validID(m.ID) || m.TotalBytes != p.content.Media[index].SizeBytes || m.UploadedBytes < 0 || m.UploadedBytes > m.TotalBytes || m.NextSegment != int((m.UploadedBytes+chunkSize-1)/chunkSize) || m.NextSegment > 10000 {
		return invalid("x_invalid_checkpoint", "An X media checkpoint has inconsistent upload positions.")
	}
	if m.UploadedBytes > 0 {
		digest, err := hex.DecodeString(m.PrefixSHA256)
		if err != nil || len(digest) != sha256.Size {
			return invalid("x_invalid_checkpoint", "An X media checkpoint has no valid source digest.")
		}
	}
	switch m.Stage {
	case "initialization_unconfirmed":
		if m.UploadedBytes != 0 || m.NextSegment != 0 {
			return invalid("x_invalid_checkpoint", "An unconfirmed X upload contains acknowledged bytes.")
		}
	case "appending":
		if m.UploadedBytes != m.TotalBytes && m.UploadedBytes%chunkSize != 0 {
			return invalid("x_invalid_checkpoint", "An X media checkpoint is not on a chunk boundary.")
		}
	case "appended", "finalizing", "processing", "ready", "failed":
		if m.UploadedBytes != m.TotalBytes {
			return invalid("x_invalid_checkpoint", "An X media checkpoint finalized an incomplete upload.")
		}
	default:
		return invalid("x_invalid_checkpoint", "An X media checkpoint has an unknown stage.")
	}
	return nil
}

func (p *publication) prepareMedia(index int) (time.Duration, error) {
	m := &p.data.Media[index]
	if m.Stage == "initialization_unconfirmed" {
		*m = mediaState{}
		if err := p.save("x.upload", false); err != nil {
			return 0, err
		}
	}
	if m.ID != "" && !m.ExpiresAt.IsZero() && !m.ExpiresAt.After(p.client.now().Add(time.Minute)) {
		// Refresh an expiring upload before it can reach the public mutation.
		*m = mediaState{}
		if err := p.save("x.upload", false); err != nil {
			return 0, err
		}
	}
	if m.Stage == "failed" {
		return 0, mediaProcessingFailed()
	}
	if m.Stage == "ready" {
		return 0, nil
	}
	if m.Stage == "processing" || m.Stage == "finalizing" {
		if wait := m.NextPollAt.Sub(p.client.now()); wait > 0 {
			return wait, nil
		}
		r, requestErr := p.client.request(p.ctx, http.MethodGet, "/2/media/upload?command=STATUS&media_id="+m.ID, "", nil, p.token.AccessToken, false)
		if r.status == http.StatusNotFound || r.status == http.StatusGone {
			*m = mediaState{}
			if err := p.save("x.upload", false); err != nil {
				return 0, err
			}
		} else if r.status == http.StatusBadRequest && m.Stage == "finalizing" {
			// A crash can occur after saving "finalizing" but before sending the
			// request. A status rejection then permits another non-public finalize.
			m.Stage, m.NextPollAt = "appended", time.Time{}
			if err := p.save("x.upload", false); err != nil {
				return 0, err
			}
		} else {
			if err := p.client.checkResponse(r, requestErr, false); err != nil {
				return 0, err
			}
			return p.applyProcessing(index, r)
		}
	}
	if m.ID == "" || m.Stage == "appending" {
		if err := p.upload(index); err != nil {
			return 0, err
		}
	}
	if m.Stage != "appended" {
		return 0, invalid("x_invalid_checkpoint", "The X upload is not ready for finalization.")
	}
	m.Stage = "finalizing"
	if err := p.save("x.upload", false); err != nil {
		return 0, err
	}
	r, requestErr := p.client.request(p.ctx, http.MethodPost, "/2/media/upload/"+m.ID+"/finalize", "", nil, p.token.AccessToken, false)
	if err := p.client.checkResponse(r, requestErr, false); err != nil {
		return 0, err
	}
	return p.applyProcessing(index, r)
}

func (p *publication) upload(index int) error {
	if p.client.config.Media == nil {
		return invalid("x_media_fetcher_missing", "A secure source-media fetcher is required for X attachments.")
	}
	asset := p.content.Media[index]
	m := &p.data.Media[index]
	source, size, err := p.client.config.Media.Open(p.ctx, asset)
	if err != nil {
		if p.ctx.Err() != nil {
			return p.ctx.Err()
		}
		return &provider.Error{Code: "x_media_fetch_failed", Message: "The approved media source could not be opened. Ensure it remains available through publication.", Retryable: true}
	}
	if source == nil {
		return invalid("x_media_fetch_failed", "The media source returned an empty reader.")
	}
	defer source.Close()
	stopClose := context.AfterFunc(p.ctx, func() { _ = source.Close() })
	defer stopClose()
	if size >= 0 && size != asset.SizeBytes {
		return invalid("x_media_size_changed", "The source's size differs from its declared size_bytes.")
	}
	if m.ID == "" {
		category, mediaType, err := mediaKind(asset.MIMEType)
		if err != nil {
			return err
		}
		body, _ := json.Marshal(struct {
			MediaType     string `json:"media_type"`
			TotalBytes    int64  `json:"total_bytes"`
			MediaCategory string `json:"media_category"`
		}{mediaType, asset.SizeBytes, category})
		r, requestErr := p.client.request(p.ctx, http.MethodPost, "/2/media/upload/initialize", "application/json", bytes.NewReader(body), p.token.AccessToken, false)
		wire, mediaErr := p.client.decodeMediaResponse(r)
		operationErr := p.client.checkResponse(r, requestErr, false)
		decoded := wire.Data != nil && validID(wire.Data.ID)
		if decoded {
			m.ID, m.Stage, m.TotalBytes = wire.Data.ID, "appending", asset.SizeBytes
			if operationErr != nil || mediaErr != nil {
				m.Stage = "initialization_unconfirmed"
			}
			p.updateExpiry(m, wire.Data)
			// A returned remote identifier must be saved before any append.
			if err := p.save("x.upload", false); err != nil {
				return err
			}
		}
		if operationErr != nil {
			return operationErr
		}
		if mediaErr != nil {
			return mediaErr
		}
		if !decoded {
			return &provider.Error{Code: "x_invalid_media_response", Message: "X did not return a usable media upload identifier.", Retryable: true}
		}
	}
	// The fetcher interface does not promise range support. Reopen the source
	// and verify its acknowledged prefix before resuming, using bounded memory.
	digest := sha256.New()
	if m.UploadedBytes > 0 {
		if _, err := io.CopyN(digest, source, m.UploadedBytes); err != nil {
			return p.sourceReadError(err)
		}
		if hex.EncodeToString(digest.Sum(nil)) != m.PrefixSHA256 {
			return invalid("x_media_changed", "The source media changed after upload began. Submit a new post with an immutable source asset.")
		}
	}
	buffer := make([]byte, chunkSize)
	for m.UploadedBytes < m.TotalBytes {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if !m.ExpiresAt.IsZero() && !m.ExpiresAt.After(p.client.now()) {
			*m = mediaState{}
			if err := p.save("x.upload", false); err != nil {
				return err
			}
			return &provider.Error{Code: "x_media_expired", Message: "The X upload session expired. A new upload is required before publication.", Retryable: true}
		}
		remaining := m.TotalBytes - m.UploadedBytes
		length := int64(chunkSize)
		if remaining < length {
			length = remaining
		}
		chunk := buffer[:int(length)]
		if _, err := io.ReadFull(source, chunk); err != nil {
			return p.sourceReadError(err)
		}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("segment_index", strconv.Itoa(m.NextSegment)); err != nil {
			return err
		}
		part, err := writer.CreateFormFile("media", "chunk")
		if err != nil {
			return err
		}
		if _, err := part.Write(chunk); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
		r, requestErr := p.client.request(p.ctx, http.MethodPost, "/2/media/upload/"+m.ID+"/append", writer.FormDataContentType(), &body, p.token.AccessToken, false)
		if err := p.client.checkResponse(r, requestErr, false); err != nil {
			return err
		}
		wire, err := p.client.decodeMediaResponse(r)
		if err != nil {
			return err
		}
		if wire.Data.ID != "" && wire.Data.ID != m.ID {
			return &provider.Error{Code: "x_invalid_media_response", Message: "X acknowledged a different media upload. Publication has stopped.", Retryable: true}
		}
		_, _ = digest.Write(chunk)
		m.UploadedBytes += length
		m.NextSegment++
		m.PrefixSHA256 = hex.EncodeToString(digest.Sum(nil))
		p.updateExpiry(m, wire.Data)
		if err := p.save("x.upload", false); err != nil {
			return err
		}
	}
	var extra [1]byte
	n, err := io.ReadFull(source, extra[:])
	if n != 0 {
		return invalid("x_media_size_changed", "The source contains more bytes than its declared size_bytes.")
	}
	if err != io.EOF {
		return p.sourceReadError(err)
	}
	m.Stage = "appended"
	return p.save("x.upload", false)
}

func (p *publication) sourceReadError(err error) error {
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return invalid("x_media_size_changed", "The media source ended before its declared size_bytes.")
	}
	return &provider.Error{Code: "x_media_read_failed", Message: "Reading the source media was interrupted; the acknowledged upload can be resumed.", Retryable: true}
}

func (p *publication) updateExpiry(m *mediaState, result *mediaResult) {
	if result.ExpiresAfterSecs > 0 && result.ExpiresAfterSecs <= 365*24*60*60 {
		m.ExpiresAt = p.client.now().UTC().Add(time.Duration(result.ExpiresAfterSecs) * time.Second)
	}
	if result.ExpiresAt > 0 {
		at := time.Unix(result.ExpiresAt, 0).UTC()
		// The reference describes a timestamp. Do not mistake an undocumented
		// millisecond value or an outlier for a session lasting millennia.
		if at.Before(p.client.now().Add(366 * 24 * time.Hour)) {
			m.ExpiresAt = at
		}
	}
}

func (p *publication) applyProcessing(index int, r response) (time.Duration, error) {
	m := &p.data.Media[index]
	wire, err := p.client.decodeMediaResponse(r)
	if err != nil {
		return 0, err
	}
	if (wire.Data.ID == "" && wire.Data.Processing == nil) || (wire.Data.ID != "" && wire.Data.ID != m.ID) {
		return 0, &provider.Error{Code: "x_invalid_media_status", Message: "X returned an incomplete media processing response.", Retryable: true}
	}
	p.updateExpiry(m, wire.Data)
	if wire.Data.Processing == nil || wire.Data.Processing.State == "succeeded" {
		m.Stage, m.NextPollAt = "ready", time.Time{}
		return 0, p.save("x.upload", false)
	}
	switch wire.Data.Processing.State {
	case "pending", "in_progress":
		seconds := wire.Data.Processing.CheckAfterSecs
		if seconds <= 0 {
			seconds = 1
		}
		if seconds > 24*60*60 {
			seconds = 24 * 60 * 60
		}
		wait := time.Duration(seconds) * time.Second
		m.Stage, m.NextPollAt = "processing", p.client.now().UTC().Add(wait)
		if err := p.save("x.processing", false); err != nil {
			return 0, err
		}
		return wait, nil
	case "failed":
		m.Stage, m.NextPollAt = "failed", time.Time{}
		if err := p.save("x.upload", false); err != nil {
			return 0, err
		}
		return 0, mediaProcessingFailed()
	default:
		return 0, &provider.Error{Code: "x_invalid_media_status", Message: "X returned an unknown media processing state.", Retryable: true}
	}
}

func (p *publication) createPost() (model.Outcome, error) {
	type postMedia struct {
		IDs []string `json:"media_ids"`
	}
	var attachments *postMedia
	if len(p.data.Media) > 0 {
		attachments = &postMedia{IDs: make([]string, 0, len(p.data.Media))}
		for i, media := range p.data.Media {
			if media.Stage != "ready" || !validID(media.ID) {
				return p.outcome(0), invalid("x_media_not_ready", "X media processing must finish before publication.")
			}
			if !media.ExpiresAt.IsZero() && !media.ExpiresAt.After(p.client.now().Add(time.Minute)) {
				p.data.Media[i] = mediaState{}
				if err := p.save("x.upload", false); err != nil {
					return p.outcome(0), err
				}
				return p.outcome(time.Second), nil
			}
			if p.content.Media[i].AltText != "" && !media.AltTextApplied {
				return p.outcome(0), invalid("x_media_not_ready", "Media descriptions must be attached before X publication.")
			}
			attachments.IDs = append(attachments.IDs, media.ID)
		}
	}
	type postReply struct {
		ID string `json:"in_reply_to_tweet_id"`
	}
	var reply *postReply
	if p.content.Reply != nil {
		reply = &postReply{ID: p.content.Reply.RemoteID}
	}
	body, err := json.Marshal(struct {
		Text  string     `json:"text"`
		Media *postMedia `json:"media,omitempty"`
		Reply *postReply `json:"reply,omitempty"`
	}{Text: p.content.Text, Media: attachments, Reply: reply})
	if err != nil {
		return p.outcome(0), invalid("x_invalid_content", "The X post payload could not be encoded.")
	}
	if err := p.ctx.Err(); err != nil {
		return p.outcome(0), err
	}
	if deadline := p.content.PublishBefore; deadline != nil && !deadline.After(p.client.now()) {
		return p.outcome(0), invalid("x_publication_window_expired", "The approved publication window has ended.")
	}
	if err := p.save("x.create", true); err != nil {
		return p.outcome(0), err
	}
	r, requestErr := p.client.request(p.ctx, http.MethodPost, "/2/tweets", "application/json", bytes.NewReader(body), p.token.AccessToken, false)
	var wire struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(r.body, &wire) == nil && validID(wire.Data.ID) {
		p.data.PostID = wire.Data.ID
		receipt, err := p.progress("x.published", false)
		if err != nil {
			// All fields were already JSON serializable before the request.
			return published(wire.Data.ID, p.current), nil
		}
		out := published(wire.Data.ID, receipt)
		if err := p.checkpoint(receipt); err != nil {
			// No further remote action is performed. Let the worker's fenced
			// completion attempt retain the known receipt rather than retry it.
			out.Warnings = []string{"X confirmed publication, but the intermediate receipt checkpoint could not be saved."}
		}
		return out, nil
	}
	// These HTTP client errors are definitive rejections. A request timeout,
	// a gateway error, a lost response, and any incomplete success are not.
	if r.status >= 400 && r.status < 500 && r.status != http.StatusRequestTimeout {
		if err := p.save("x.ready", false); err != nil {
			return p.outcome(0), &provider.Error{Code: "x_rejection_checkpoint_failed", Message: "X rejected the request, but the safe outcome could not be saved. Reconcile before retrying.", Ambiguous: true}
		}
		return p.outcome(0), p.client.responseError(r, false)
	}
	_ = requestErr // Its text may include credentials or URLs; uncertainty is enough.
	return p.outcome(0), uncertain()
}

func (p *publication) progress(stage string, unsafe bool) (model.Progress, error) {
	data, err := json.Marshal(p.data)
	if err != nil {
		return model.Progress{}, fmt.Errorf("encode X upload checkpoint: %w", err)
	}
	return model.Progress{Stage: stage, Data: data, Unsafe: unsafe}, nil
}

func (p *publication) save(stage string, unsafe bool) error {
	progress, err := p.progress(stage, unsafe)
	if err != nil {
		return err
	}
	if err := p.checkpoint(progress); err != nil {
		return fmt.Errorf("save X upload checkpoint: %w", err)
	}
	p.current = progress
	return nil
}

func (p *publication) outcome(wait time.Duration) model.Outcome {
	return model.Outcome{Progress: p.current, PollAfter: wait}
}

func published(id string, progress model.Progress) model.Outcome {
	return model.Outcome{Done: true, RemoteID: id, RemoteURL: "https://x.com/i/status/" + id, Progress: progress}
}

func uncertain() *provider.Error {
	return &provider.Error{Code: "x_publish_uncertain", Message: "X may have accepted the post. Verify the account and reconcile the result before retrying.", Ambiguous: true}
}

func mediaProcessingFailed() *provider.Error {
	return invalid("x_media_processing_failed", "X could not process the media. Check the file's codec, dimensions, duration, and account limits.")
}
