package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
)

var decimalXID = regexp.MustCompile(`^[1-9][0-9]{0,24}$`)

// ConfirmedXReply never accepts an arbitrary remote ID or an unconfirmed send.
// The original receipt must bind content, tenant, account and provider identity.
func ConfirmedXReply(parent Post, account Account) (*XReply, error) {
	if parent.Owner != account.Owner || account.Platform != X || account.Status != "active" || !decimalXID.MatchString(account.Identity.ID) {
		return nil, errors.New("x_thread_parent_unavailable")
	}
	var reply *XReply
	for _, target := range parent.Targets {
		if target.AccountID != account.ID || target.Platform != X {
			continue
		}
		var receipt struct {
			Version     int    `json:"version"`
			OwnerID     string `json:"owner_id"`
			PostID      string `json:"post_id"`
			ContentHash string `json:"content_hash"`
		}
		encoded, err := json.Marshal(target.Content)
		if err != nil {
			return nil, errors.New("x_thread_parent_unavailable")
		}
		digest := sha256.Sum256(encoded)
		if reply != nil || target.Status != "published" || target.Progress.Unsafe || target.Progress.Stage != "x.published" || !decimalXID.MatchString(target.RemoteID) || json.Unmarshal(target.Progress.Data, &receipt) != nil || receipt.Version != 1 || receipt.OwnerID != account.Identity.ID || receipt.PostID != target.RemoteID || receipt.ContentHash != hex.EncodeToString(digest[:]) {
			return nil, errors.New("x_thread_parent_unavailable")
		}
		reply = &XReply{PostID: parent.ID, RemoteID: target.RemoteID, OwnerID: account.Identity.ID}
	}
	if reply == nil {
		return nil, errors.New("x_thread_parent_unavailable")
	}
	return reply, nil
}
