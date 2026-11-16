package x

import (
	"testing"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/security"
)

func TestMPEGTSUsesTheSamePolicyThroughXValidation(t *testing.T) {
	media := model.Media{URL: "https://media.example.com/video.ts", MIMEType: "video/mp2t", SizeBytes: 1880,
		DurationSeconds: 1, Width: 1280, Height: 720}
	fetcher, err := security.NewMediaFetcher([]string{"media.example.com"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := fetcher.Validate(media); err != nil {
		t.Fatalf("shared policy rejected X video: %v", err)
	}
	c := New(provider.Config{})
	if err := c.Validate(model.Content{Media: []model.Media{media}}, model.Capabilities{CanPublish: true}); err != nil {
		t.Fatalf("X rejected shared-policy-approved MPEG-TS: %v", err)
	}
	category, mime, err := mediaKind(media.MIMEType)
	if err != nil || category != "tweet_video" || mime != media.MIMEType {
		t.Fatalf("upload classification changed media type: %s %s %v", category, mime, err)
	}
}
