package tiktok

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/url"
	"strings"
	"unicode/utf8"

	"socialpublisher/internal/model"
)

const (
	maxVideoBytes int64 = 4_000_000_000
	maxPhotoBytes int64 = 20_000_000
)

// Options are per-target TikTok choices. Required booleans are pointers so
// omission cannot silently become consent or an interaction/disclosure choice.
type Options struct {
	PrivacyLevel          string  `json:"privacy_level"`
	Consent               *bool   `json:"consent"`
	PreviewConfirmed      *bool   `json:"preview_confirmed"`
	AllowComment          *bool   `json:"allow_comment"`
	AllowDuet             *bool   `json:"allow_duet,omitempty"`
	AllowStitch           *bool   `json:"allow_stitch,omitempty"`
	BrandContentToggle    *bool   `json:"brand_content_toggle"`
	BrandOrganicToggle    *bool   `json:"brand_organic_toggle"`
	IsAIGC                *bool   `json:"is_aigc,omitempty"`
	Title                 *string `json:"title,omitempty"`
	PhotoCoverIndex       *int    `json:"photo_cover_index,omitempty"`
	AutoAddMusic          *bool   `json:"auto_add_music,omitempty"`
	VideoCoverTimestampMS *int64  `json:"video_cover_timestamp_ms,omitempty"`
}

func (c *Client) Validate(content model.Content, caps model.Capabilities) error {
	options, err := decodeOptions(content.Options)
	if err != nil {
		return err
	}
	if !caps.CanPublish {
		return localError("publishing_unavailable", "TikTok cannot currently publish for this account")
	}
	if !trueValue(options.Consent) || !trueValue(options.PreviewConfirmed) {
		return localError("consent_required", "The creator must preview this content and explicitly consent to its TikTok publication")
	}
	if !knownPrivacy(options.PrivacyLevel) || !contains(caps.PrivacyLevels, options.PrivacyLevel) {
		return localError("privacy_selection_required", "The creator must select a currently available TikTok privacy level")
	}
	if options.AllowComment == nil || options.BrandContentToggle == nil || options.BrandOrganicToggle == nil {
		return localError("explicit_choices_required", "TikTok interaction and commercial disclosure choices must be explicitly supplied")
	}
	if trueValue(options.AllowComment) && caps.CommentDisabled {
		return localError("interaction_unavailable", "TikTok comments are disabled for this account")
	}
	if trueValue(options.BrandContentToggle) && options.PrivacyLevel == "SELF_ONLY" {
		return localError("branded_content_private", "TikTok third-party branded content cannot use only-me visibility")
	}
	if !c.config.Audited && (options.PrivacyLevel != "SELF_ONLY" || caps.AccountType != "private") {
		return localError("unaudited_client_can_only_post_to_private_accounts", "Unaudited TikTok applications require a private account and only-me visibility")
	}
	if len(content.Media) == 0 || len(content.Media) > 35 {
		return localError("invalid_media_count", "TikTok requires one video or between one and thirty-five photos")
	}
	for _, media := range content.Media {
		if err := validateSource(media); err != nil {
			return err
		}
	}
	if isVideo(content.Media[0]) {
		return validateVideo(content, options, caps)
	}
	return validatePhotos(content, options)
}

func validateVideo(content model.Content, options Options, caps model.Capabilities) error {
	if len(content.Media) != 1 {
		return localError("invalid_media_count", "TikTok video posts require exactly one video")
	}
	if options.AllowDuet == nil || options.AllowStitch == nil {
		return localError("explicit_choices_required", "TikTok video duet and stitch choices must be explicitly supplied")
	}
	if (trueValue(options.AllowDuet) && caps.DuetDisabled) || (trueValue(options.AllowStitch) && caps.StitchDisabled) {
		return localError("interaction_unavailable", "A selected TikTok video interaction is disabled for this account")
	}
	if options.Title != nil || options.PhotoCoverIndex != nil || options.AutoAddMusic != nil {
		return localError("unsupported_option", "Photo-only TikTok options cannot be used for a video")
	}
	if !utf8.ValidString(content.Text) || utf16Length(content.Text) > 2200 {
		return localError("caption_too_long", "TikTok video captions must contain at most 2200 UTF-16 code units")
	}
	media := content.Media[0]
	if !isVideo(media) || media.SizeBytes > maxVideoBytes {
		return localError("invalid_video", "TikTok video must use a supported format and fit the configured 4 GB limit")
	}
	if math.IsNaN(media.DurationSeconds) || math.IsInf(media.DurationSeconds, 0) || media.DurationSeconds <= 0 ||
		caps.MaxVideoDurationSeconds <= 0 || media.DurationSeconds > float64(caps.MaxVideoDurationSeconds) {
		return localError("invalid_video_duration", "TikTok video duration must be known and fit the creator's current duration limit")
	}
	if media.Width < 360 || media.Height < 360 || media.Width > 4096 || media.Height > 4096 {
		return localError("invalid_video_dimensions", "TikTok video dimensions must each be between 360 and 4096 pixels")
	}
	if options.VideoCoverTimestampMS != nil {
		value := *options.VideoCoverTimestampMS
		if value < 0 || value > math.MaxInt32 || float64(value) >= media.DurationSeconds*1000 {
			return localError("invalid_cover", "TikTok video cover timestamp must identify a frame within the video")
		}
	}
	return nil
}

func validatePhotos(content model.Content, options Options) error {
	if options.AllowDuet != nil || options.AllowStitch != nil || options.VideoCoverTimestampMS != nil {
		return localError("unsupported_option", "TikTok photo posts cannot include video-only options")
	}
	if !utf8.ValidString(content.Text) || utf16Length(content.Text) > 4000 {
		return localError("description_too_long", "TikTok photo descriptions must contain at most 4000 UTF-16 code units")
	}
	if options.Title != nil && (!utf8.ValidString(*options.Title) || utf16Length(*options.Title) > 90) {
		return localError("title_too_long", "TikTok photo titles must contain at most 90 UTF-16 code units")
	}
	if options.PhotoCoverIndex != nil && (*options.PhotoCoverIndex < 0 || *options.PhotoCoverIndex >= len(content.Media)) {
		return localError("invalid_cover", "TikTok photo cover index must reference one of the selected photos")
	}
	for _, media := range content.Media {
		if (media.MIMEType != "image/jpeg" && media.MIMEType != "image/webp") || media.SizeBytes > maxPhotoBytes {
			return localError("invalid_photo", "TikTok photos must be JPEG or WebP and fit the configured 20 MB per-photo limit")
		}
		// TikTok documents "maximum 1080p" without an orientation-specific pair.
		// This is the service's conservative local envelope, documented as such.
		shorter, longer := media.Width, media.Height
		if shorter > longer {
			shorter, longer = longer, shorter
		}
		if shorter <= 0 || shorter > 1080 || longer > 1920 {
			return localError("invalid_photo_dimensions", "TikTok photo dimensions must be known and fit this service's 1080 by 1920 envelope in either orientation")
		}
	}
	return nil
}

func validateSource(media model.Media) error {
	if media.SizeBytes <= 0 {
		return localError("media_size_required", "TikTok media requires a positive declared byte size")
	}
	u, err := url.Parse(media.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.Fragment != "" {
		return localError("invalid_media_url", "TikTok media must use an HTTPS URL without credentials or a fragment")
	}
	if net.ParseIP(u.Hostname()) != nil || strings.Contains(u.Hostname(), ":") || strings.HasSuffix(u.Hostname(), ".") || (u.Port() != "" && u.Port() != "443") {
		return localError("invalid_media_url", "TikTok media must use an approved DNS hostname on HTTPS port 443")
	}
	// Exact-host allowlisting, DNS checks, URL verification, and URL longevity
	// belong to the service's shared media policy and deployment configuration.
	return nil
}

func decodeOptions(raw json.RawMessage) (Options, error) {
	var options Options
	if len(raw) == 0 || len(raw) > 64<<10 {
		return options, localError("invalid_options", "Explicit TikTok posting options are required")
	}
	// encoding/json otherwise accepts duplicate keys. Reject them before the
	// typed decode so consent and privacy cannot have competing representations.
	check := json.NewDecoder(bytes.NewReader(raw))
	start, err := check.Token()
	if err != nil || start != json.Delim('{') {
		return options, localError("invalid_options", "TikTok options must be a JSON object")
	}
	seen := make(map[string]bool)
	for check.More() {
		token, err := check.Token()
		if err != nil {
			return options, localError("invalid_options", "TikTok options contain invalid JSON")
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return options, localError("invalid_options", "TikTok option names must be unique")
		}
		if !knownOption(key) {
			return options, localError("invalid_options", "TikTok options contain an unsupported or noncanonical field name")
		}
		seen[key] = true
		var value json.RawMessage
		if check.Decode(&value) != nil {
			return options, localError("invalid_options", "TikTok options contain invalid JSON")
		}
	}
	if _, err := check.Token(); err != nil {
		return options, localError("invalid_options", "TikTok options contain invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&options) != nil {
		return options, localError("invalid_options", "TikTok options contain unsupported fields or invalid values")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return options, localError("invalid_options", "TikTok options must contain exactly one JSON object")
	}
	return options, nil
}

func knownOption(key string) bool {
	// encoding/json matches struct fields case-insensitively. Enforce the
	// published local JSON spelling before decoding the user's choices.
	switch key {
	case "privacy_level", "consent", "preview_confirmed", "allow_comment", "allow_duet", "allow_stitch", "brand_content_toggle", "brand_organic_toggle", "is_aigc", "title", "photo_cover_index", "auto_add_music", "video_cover_timestamp_ms":
		return true
	default:
		return false
	}
}

func isVideo(media model.Media) bool {
	return media.MIMEType == "video/mp4" || media.MIMEType == "video/quicktime" || media.MIMEType == "video/webm"
}

func knownPrivacy(value string) bool {
	switch value {
	case "PUBLIC_TO_EVERYONE", "MUTUAL_FOLLOW_FRIENDS", "FOLLOWER_OF_CREATOR", "SELF_ONLY":
		return true
	default:
		return false
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func trueValue(value *bool) bool { return value != nil && *value }

func utf16Length(value string) int {
	size := 0
	for _, r := range value {
		size++
		if r > 0xffff {
			size++
		}
	}
	return size
}
