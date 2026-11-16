package instagram

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"mime"
	"net/url"
	"strings"
	"unicode/utf8"

	"socialpublisher/internal/model"
)

const (
	maxImageBytes = 8_000_000
	maxVideoBytes = 1_000_000_000
)

type options struct {
	MediaType   string
	ShareToFeed *bool
}

func parseOptions(raw json.RawMessage) (options, error) {
	var out options
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return out, nil
	}
	if len(raw) > 16*1024 {
		return out, invalidOptions()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return out, invalidOptions()
	}
	seen := make(map[string]bool)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return out, invalidOptions()
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return out, invalidOptions()
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return out, invalidOptions()
		}
		switch key {
		case "media_type":
			if err := json.Unmarshal(value, &out.MediaType); err != nil {
				return out, invalidOptions()
			}
		case "share_to_feed":
			var share bool
			if err := json.Unmarshal(value, &share); err != nil {
				return out, invalidOptions()
			}
			out.ShareToFeed = &share
		default:
			return out, invalidOptions()
		}
	}
	if last, err := d.Token(); err != nil || last != json.Delim('}') {
		return out, invalidOptions()
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return out, invalidOptions()
	}
	return out, nil
}

func invalidOptions() error {
	return localError("instagram_unsupported_options", "Instagram options must contain only media_type and the optional boolean share_to_feed; duplicate or invalid values are not accepted.")
}

func (c *Client) Validate(content model.Content, caps model.Capabilities) error {
	if !caps.CanPublish {
		return localError("instagram_publishing_unavailable", "This Instagram connection does not have publishing access.")
	}
	if !utf8.ValidString(content.Text) || utf8.RuneCountInString(content.Text) > 2200 {
		return localError("instagram_caption_too_long", "Instagram captions must be valid text with at most 2,200 characters.")
	}
	opt, err := parseOptions(content.Options)
	if err != nil {
		return err
	}
	if opt.MediaType == "STORIES" {
		return localError("instagram_stories_unsupported", "Instagram Stories are not supported by this adapter.")
	}
	if len(content.Media) == 0 || len(content.Media) > 10 {
		return localError("instagram_media_required", "Instagram requires one media item or a carousel of two to ten items.")
	}
	kind := inferredType(content)
	if opt.MediaType != "" && opt.MediaType != kind {
		return localError("instagram_media_type_mismatch", "Instagram media_type must match the supplied media: IMAGE, REELS, or CAROUSEL.")
	}
	if kind != "REELS" && opt.ShareToFeed != nil {
		return localError("instagram_unsupported_options", "share_to_feed is supported only for Instagram Reels.")
	}
	for _, media := range content.Media {
		if !validMediaURL(media.URL) {
			return localError("instagram_invalid_media_url", "Instagram media must use an absolute HTTPS URL without credentials or a fragment.")
		}
		if media.SizeBytes <= 0 || media.Width < 0 || media.Height < 0 {
			return localError("instagram_invalid_media_metadata", "Instagram media requires a positive byte size and nonnegative dimensions.")
		}
		mediaType := mediaMIME(media.MIMEType)
		switch mediaType {
		case "image/jpeg":
			if media.SizeBytes > maxImageBytes {
				return localError("instagram_image_too_large", "Instagram images must be JPEG files no larger than 8,000,000 bytes.")
			}
			if media.Width > 0 && media.Height > 0 {
				ratio := float64(media.Width) / float64(media.Height)
				if ratio < 0.8 || ratio > 1.91 {
					return localError("instagram_image_aspect_ratio", "Instagram feed images must have an aspect ratio between 4:5 and 1.91:1.")
				}
			}
			if !utf8.ValidString(media.AltText) || utf8.RuneCountInString(media.AltText) > 1000 {
				return localError("instagram_alt_text_invalid", "Instagram image alt text must contain at most 1,000 characters.")
			}
		case "video/mp4", "video/quicktime":
			maxDuration := 900
			if caps.MaxVideoDurationSeconds > 0 {
				maxDuration = min(maxDuration, caps.MaxVideoDurationSeconds)
			}
			if kind == "CAROUSEL" {
				maxDuration = min(maxDuration, 60)
			}
			if media.SizeBytes > maxVideoBytes || math.IsNaN(media.DurationSeconds) || math.IsInf(media.DurationSeconds, 0) || media.DurationSeconds < 3 || media.DurationSeconds > float64(maxDuration) {
				return localError("instagram_video_limits", "Instagram video exceeds the supported byte-size or duration limits; use a 3–900 second Reel or a 3–60 second carousel video, up to 1,000,000,000 bytes.")
			}
			if media.Width > 1920 {
				return localError("instagram_video_dimensions", "Instagram video width must not exceed 1,920 pixels.")
			}
			if media.AltText != "" {
				return localError("instagram_video_alt_text_unsupported", "Instagram alt text is supported for images only.")
			}
		default:
			return localError("instagram_media_format", "Instagram media must be JPEG images or MP4/MOV videos.")
		}
	}
	return nil
}

func inferredType(content model.Content) string {
	if len(content.Media) > 1 {
		return "CAROUSEL"
	}
	if len(content.Media) == 1 && strings.HasPrefix(mediaMIME(content.Media[0].MIMEType), "video/") {
		return "REELS"
	}
	return "IMAGE"
}

func mediaMIME(value string) string {
	m, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return strings.ToLower(m)
}

func validMediaURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 8192 || strings.ContainsAny(raw, "\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && u.Opaque == ""
}
