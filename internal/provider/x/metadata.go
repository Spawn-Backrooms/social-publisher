package x

import (
	"bytes"
	"encoding/json"
	"net/http"

	"socialpublisher/internal/provider"
)

func (p *publication) applyAltText(index int) error {
	m := &p.data.Media[index]
	var payload struct {
		ID       string `json:"id"`
		Metadata struct {
			AltText struct {
				Text string `json:"text"`
			} `json:"alt_text"`
		} `json:"metadata"`
	}
	payload.ID = m.ID
	payload.Metadata.AltText.Text = p.content.Media[index].AltText
	body, _ := json.Marshal(payload)
	r, requestErr := p.client.request(p.ctx, http.MethodPost, "/2/media/metadata", "application/json", bytes.NewReader(body), p.token.AccessToken, false)
	if err := p.client.checkResponse(r, requestErr, false); err != nil {
		return err
	}
	wire, err := p.client.decodeMediaResponse(r)
	if err != nil {
		return err
	}
	if wire.Data.ID != m.ID {
		return &provider.Error{Code: "x_invalid_metadata_response", Message: "X did not confirm the media description. The post has not been published.", Retryable: true}
	}
	m.AltTextApplied = true
	return p.save("x.upload", false)
}
