package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// IsTokenRejectionBody reports whether a complete JSON object contains no
// credential fields, including wrapped entries. This is only a prerequisite:
// the caller must separately establish an explicit HTTP/OAuth rejection before
// claiming RefreshNotApplied. Malformed, duplicate or contradictory fields can
// never supply proof that a stored grant was not rotated.
func IsTokenRejectionBody(body []byte) bool {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	first, err := d.Token()
	if err != nil || first != json.Delim('{') || !tokenRejectionObject(d, 0) {
		return false
	}
	_, err = d.Token()
	return err == io.EOF
}

func tokenRejectionObject(d *json.Decoder, depth int) bool {
	if depth > 32 {
		return false
	}
	keys := make(map[string]bool)
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || keys[key] || strings.EqualFold(key, "access_token") || strings.EqualFold(key, "refresh_token") {
			return false
		}
		keys[key] = true
		if !tokenRejectionValue(d, depth+1) {
			return false
		}
	}
	last, err := d.Token()
	return err == nil && last == json.Delim('}')
}

func tokenRejectionValue(d *json.Decoder, depth int) bool {
	if depth > 32 {
		return false
	}
	value, err := d.Token()
	if err != nil {
		return false
	}
	if delim, ok := value.(json.Delim); ok {
		switch delim {
		case '{':
			return tokenRejectionObject(d, depth)
		case '[':
			for d.More() {
				if !tokenRejectionValue(d, depth+1) {
					return false
				}
			}
			last, err := d.Token()
			return err == nil && last == json.Delim(']')
		default:
			return false
		}
	}
	return true
}
