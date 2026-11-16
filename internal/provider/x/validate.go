package x

import (
	"encoding/json"
	"math"
	"mime"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"socialpublisher/internal/model"
)

const (
	// Decimal MB deliberately stays below either interpretation of the
	// provider's published 5 MB / 15 MB attachment limits.
	maxImageBytes = int64(5_000_000)
	maxGIFBytes   = int64(15_000_000)
	maxTextWeight = 280
)

// This is intentionally not advertised as a twitter-text implementation. The
// URL candidate expression is an approximation, and there is no NFC/emoji
// normalization or complete TLD database in the standard-library adapter.
var urlCandidate = regexp.MustCompile(`(?i)(?:https?://|www\.)[^\s<>]+|(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,63}(?:[/:?#][^\s<>]*)?`)

func (c *Client) Validate(content model.Content, caps model.Capabilities) error {
	if !caps.CanPublish {
		return invalid("x_publish_unavailable", "This X connection does not have publishing permission.")
	}
	if len(content.Options) > 0 {
		var options map[string]json.RawMessage
		if err := json.Unmarshal(content.Options, &options); err != nil || len(options) != 0 {
			return invalid("x_unsupported_options", "Arbitrary X options are not supported. Replies require a confirmed local publisher parent post.")
		}
	}
	if content.Reply != nil && (content.Reply.PostID == "" || len(content.Reply.PostID) > 128 || !validID(content.Reply.RemoteID) || !validID(content.Reply.OwnerID)) {
		return invalid("x_invalid_thread_parent", "Replies require a confirmed local publisher parent post.")
	}
	if !utf8.ValidString(content.Text) {
		return invalid("x_invalid_text", "X text must be valid UTF-8.")
	}
	for _, char := range content.Text {
		if (unicode.IsControl(char) && char != '\n' && char != '\r' && char != '\t') || char == '\uFFFE' || char == '\uFFFF' {
			return invalid("x_invalid_text", "X text contains an unsupported control character.")
		}
	}
	if conservativeTextWeight(content.Text) > maxTextWeight {
		return invalid("x_text_too_long", "Text exceeds this adapter's conservative 280-character weight estimate. Shorten the text; X applies its own exact Unicode and URL rules.")
	}
	if strings.TrimSpace(content.Text) == "" && len(content.Media) == 0 {
		return invalid("x_empty_content", "Provide text or at least one media attachment.")
	}
	if len(content.Media) > 4 {
		return invalid("x_too_many_media", "X supports up to four photos, one animated GIF, or one video per post.")
	}
	if len(content.Media) > 0 && len(caps.Raw) > 0 {
		var extra struct {
			MediaPermission *bool `json:"media_permission"`
		}
		if err := json.Unmarshal(caps.Raw, &extra); err == nil && extra.MediaPermission != nil && !*extra.MediaPermission {
			return invalid("x_missing_media_scope", "Reconnect this X account with media.write permission.")
		}
	}
	maxDuration := caps.MaxVideoDurationSeconds
	if maxDuration <= 0 {
		maxDuration = 1200
	}
	if maxDuration > 7500 {
		maxDuration = 7500
	}
	maxVideoBytes := int64(8 << 30)
	if maxDuration > 1200 || caps.AccountType == "premium_or_verified" {
		maxVideoBytes = 16 << 30
	}
	for _, media := range content.Media {
		u, err := url.Parse(media.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return invalid("x_invalid_media_url", "Each media attachment requires an HTTPS source URL on an approved origin.")
		}
		if media.SizeBytes <= 0 {
			return invalid("x_media_size_required", "Declare the positive size_bytes of every media attachment.")
		}
		category, _, err := mediaKind(media.MIMEType)
		if err != nil {
			return err
		}
		if !utf8.ValidString(media.AltText) || utf8.RuneCountInString(media.AltText) > 1000 {
			return invalid("x_invalid_alt_text", "Image descriptions must be valid UTF-8 and contain at most 1,000 Unicode characters.")
		}
		if media.AltText != "" && category == "tweet_video" {
			return invalid("x_video_alt_text_unsupported", "This adapter supports alt text on images and GIFs; use the post's text to describe a video.")
		}
		switch category {
		case "tweet_image":
			if media.SizeBytes > maxImageBytes {
				return invalid("x_image_too_large", "Images must be no larger than 5,000,000 bytes.")
			}
		case "tweet_gif":
			if len(content.Media) != 1 {
				return invalid("x_mixed_media", "An animated GIF must be the post's only attachment.")
			}
			if media.SizeBytes > maxGIFBytes {
				return invalid("x_gif_too_large", "Animated GIFs must be no larger than 15,000,000 bytes.")
			}
		case "tweet_video":
			if len(content.Media) != 1 {
				return invalid("x_mixed_media", "A video must be the post's only attachment.")
			}
			if media.SizeBytes > maxVideoBytes {
				return invalid("x_video_too_large", "The video exceeds this account's documented upload size limit.")
			}
			if math.IsNaN(media.DurationSeconds) || math.IsInf(media.DurationSeconds, 0) || media.DurationSeconds < 0.5 {
				return invalid("x_video_duration_required", "Declare a video duration of at least 0.5 seconds.")
			}
			if media.DurationSeconds > float64(maxDuration) {
				return invalid("x_video_too_long", "The video exceeds this account's documented duration allowance.")
			}
		}
	}
	return nil
}

func mediaKind(contentType string) (category, mediaType string, err error) {
	mediaType, _, err = mime.ParseMediaType(contentType)
	if err != nil {
		return "", "", invalid("x_unsupported_media", "Provide a supported image, GIF, or video MIME type.")
	}
	mediaType = strings.ToLower(mediaType)
	switch mediaType {
	case "image/jpeg", "image/png", "image/webp":
		category = "tweet_image"
	case "image/gif":
		category = "tweet_gif"
	case "video/mp4", "video/webm", "video/mp2t", "video/quicktime":
		category = "tweet_video"
	default:
		return "", "", invalid("x_unsupported_media", "Supported types are JPEG, PNG, WEBP, GIF, MP4, WEBM, MPEG-TS, and QuickTime video.")
	}
	return category, mediaType, nil
}

func scalarWeight(text string) int {
	weight := 0
	for _, char := range text {
		if char <= unicode.MaxASCII {
			weight++
		} else {
			weight += 2
		}
	}
	return weight
}

func conservativeTextWeight(text string) int {
	weight := scalarWeight(text)
	for _, loc := range urlCandidate.FindAllStringIndex(text, -1) {
		// Never give a long URL the shortening discount. Short recognized URLs
		// still consume at least X's documented 23 characters.
		if n := scalarWeight(text[loc[0]:loc[1]]); n < 23 {
			weight += 23 - n
		}
	}
	return weight
}
