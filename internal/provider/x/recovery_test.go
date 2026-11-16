package x

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func savedUpload(t *testing.T, content model.Content, data []byte, stage string, expires time.Time) model.Progress {
	t.Helper()
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	contentDigest := sha256.Sum256(encoded)
	prefix := sha256.Sum256(data)
	state := publishState{Version: 1, ContentHash: hex.EncodeToString(contentDigest[:]), OwnerID: "42", Media: []mediaState{{ID: "800", Stage: stage, TotalBytes: int64(len(data)), UploadedBytes: int64(len(data)), NextSegment: (len(data) + chunkSize - 1) / chunkSize, PrefixSHA256: hex.EncodeToString(prefix[:]), ExpiresAt: expires}}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return model.Progress{Stage: "x.upload", Data: raw}
}

func TestExpiredImageIsReuploadedAndAltTextIsCheckpointed(t *testing.T) {
	media := &fixtureMedia{data: bytes.Repeat([]byte{0x51}, 4096)}
	content := model.Content{Text: "An image", Media: []model.Media{{URL: "https://cdn.example/image.jpg", MIMEType: "image/jpeg", SizeBytes: int64(len(media.data)), AltText: "A mountain above a lake."}}}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	cp := new(checkpoints)
	var initCalls, appendCalls, finalizeCalls, metadataCalls, createCalls atomic.Int32
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2/media/upload/initialize":
			initCalls.Add(1)
			var body struct {
				Category string `json:"media_category"`
				MIMEType string `json:"media_type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Category != "tweet_image" || body.MIMEType != "image/jpeg" {
				t.Errorf("wrong image INIT: %+v", body)
			}
			_, _ = io.WriteString(w, `{"data":{"id":"900","expires_after_secs":86400}}`)
		case "/2/media/upload/900/append":
			appendCalls.Add(1)
			_, _ = io.WriteString(w, `{"data":{}}`)
		case "/2/media/upload/900/finalize":
			finalizeCalls.Add(1)
			_, _ = io.WriteString(w, `{"data":{"id":"900","expires_after_secs":86400}}`)
		case "/2/media/metadata":
			metadataCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			var decoded struct {
				ID       string `json:"id"`
				Metadata struct {
					AltText struct {
						Text string `json:"text"`
					} `json:"alt_text"`
				} `json:"metadata"`
			}
			if json.Unmarshal(body, &decoded) != nil || decoded.ID != "900" || decoded.Metadata.AltText.Text != content.Media[0].AltText || cp.current().Unsafe {
				t.Errorf("incorrect metadata contract: %s", body)
			}
			_, _ = io.WriteString(w, `{"data":{"id":"900","associated_metadata":{"alt_text":{"text":"A mountain above a lake."}}}}`)
		case "/2/tweets":
			var body struct {
				Media struct {
					IDs []string `json:"media_ids"`
				} `json:"media"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Media.IDs) != 1 || body.Media.IDs[0] != "900" {
				t.Errorf("expired media was attached: %+v", body)
			}
			if createCalls.Add(1) == 1 {
				w.WriteHeader(429)
				return
			}
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"data":{"id":"1000"}}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}, media)
	c.now = func() time.Time { return now }
	progress := savedUpload(t, content, media.data, "ready", now.Add(-time.Second))
	_, err := c.Publish(context.Background(), fixtureToken(), content, progress, cp.save)
	if pe := providerError(t, err); !pe.Retryable || pe.Ambiguous {
		t.Fatalf("wrong rejected post classification: %+v", pe)
	}
	out, err := c.Publish(context.Background(), fixtureToken(), content, cp.current(), cp.save)
	if err != nil || !out.Done || initCalls.Load() != 1 || appendCalls.Load() != 1 || finalizeCalls.Load() != 1 || metadataCalls.Load() != 1 || createCalls.Load() != 2 || media.opens != 1 {
		t.Fatalf("expired upload/metadata resume failed: %+v %v, init=%d append=%d finalize=%d metadata=%d create=%d opens=%d", out, err, initCalls.Load(), appendCalls.Load(), finalizeCalls.Load(), metadataCalls.Load(), createCalls.Load(), media.opens)
	}
}

func TestFinalizationCrashResumesStatusBeforeAnyMutation(t *testing.T) {
	for _, status := range []int{200, 400} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			data := []byte("fixture-image")
			content := model.Content{Media: []model.Media{{URL: "https://cdn.example/image.png", MIMEType: "image/png", SizeBytes: int64(len(data))}}}
			cp := new(checkpoints)
			var statuses, finalizes, creates atomic.Int32
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/2/media/upload":
					statuses.Add(1)
					w.WriteHeader(status)
					if status == 200 {
						_, _ = io.WriteString(w, `{"data":{"id":"800","processing_info":{"state":"succeeded"}}}`)
					}
				case "/2/media/upload/800/finalize":
					if statuses.Load() != 1 {
						t.Error("finalize was repeated before checking status")
					}
					finalizes.Add(1)
					_, _ = io.WriteString(w, `{"data":{"id":"800"}}`)
				case "/2/tweets":
					creates.Add(1)
					w.WriteHeader(201)
					_, _ = io.WriteString(w, `{"data":{"id":"1000"}}`)
				default:
					t.Errorf("unexpected request while resuming finalize: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}, nil)
			out, err := c.Publish(context.Background(), fixtureToken(), content, savedUpload(t, content, data, "finalizing", time.Now().Add(time.Hour)), cp.save)
			wantFinalize := int32(0)
			if status == 400 {
				wantFinalize = 1
			}
			if err != nil || !out.Done || statuses.Load() != 1 || finalizes.Load() != wantFinalize || creates.Load() != 1 {
				t.Fatalf("finalize recovery failed: %+v %v, status=%d finalize=%d create=%d", out, err, statuses.Load(), finalizes.Load(), creates.Load())
			}
		})
	}
}

func TestResumeRejectsChangedSourcePrefix(t *testing.T) {
	media := &fixtureMedia{data: bytes.Repeat([]byte{0x49}, chunkSize+1)}
	content := model.Content{Media: []model.Media{{URL: "https://cdn.example/movie.mp4", MIMEType: "video/mp4", SizeBytes: int64(len(media.data)), DurationSeconds: 1}}}
	cp := new(checkpoints)
	var appends atomic.Int32
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2/media/upload/initialize":
			_, _ = io.WriteString(w, `{"data":{"id":"900","expires_after_secs":86400}}`)
		case "/2/media/upload/900/append":
			if appends.Add(1) == 2 {
				w.WriteHeader(503)
				return
			}
			_, _ = io.WriteString(w, `{"data":{}}`)
		default:
			t.Errorf("unexpected request after changed media: %s", r.URL.Path)
			w.WriteHeader(400)
		}
	}, media)
	_, err := c.Publish(context.Background(), fixtureToken(), content, model.Progress{}, cp.save)
	if !providerError(t, err).Retryable {
		t.Fatalf("expected retryable append failure: %v", err)
	}
	media.changeNext = true
	_, err = c.Publish(context.Background(), fixtureToken(), content, cp.current(), cp.save)
	if pe := providerError(t, err); pe.Code != "x_media_changed" || pe.Retryable || pe.Ambiguous || appends.Load() != 2 {
		t.Fatalf("changed prefix was not rejected before append: %+v, appends=%d", pe, appends.Load())
	}
}

func TestLostCreateConnectionIsAmbiguous(t *testing.T) {
	cp := new(checkpoints)
	var calls atomic.Int32
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}, nil)
	_, err := c.Publish(context.Background(), fixtureToken(), model.Content{Text: "hello"}, model.Progress{}, cp.save)
	if pe := providerError(t, err); !pe.Ambiguous || pe.Retryable || !cp.current().Unsafe || calls.Load() != 1 {
		t.Fatalf("lost create response not preserved as uncertain: %+v calls=%d", pe, calls.Load())
	}
}

func TestKnownIDOverridesServerErrorOrTruncatedHTTPBody(t *testing.T) {
	for _, status := range []int{201, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			cp := new(checkpoints)
			c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				body := `{"data":{"id":"123"},"errors":[{"status":403,"detail":"fixture-access"}]}`
				if status == 201 {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)+20))
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}, nil)
			out, err := c.Publish(context.Background(), fixtureToken(), model.Content{Text: "hello"}, model.Progress{}, cp.save)
			if err != nil || !out.Done || out.RemoteID != "123" || cp.current().Unsafe {
				t.Fatalf("known receipt was treated as failure: %+v %v", out, err)
			}
		})
	}
}

func TestRejectedCreateWithFailedSafeCheckpointRemainsUncertain(t *testing.T) {
	cp := new(checkpoints)
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }, nil)
	_, err := c.Publish(context.Background(), fixtureToken(), model.Content{Text: "hello"}, model.Progress{}, func(progress model.Progress) error {
		if cp.current().Unsafe && !progress.Unsafe {
			return errors.New("cannot save safe rejection")
		}
		return cp.save(progress)
	})
	if pe := providerError(t, err); !pe.Ambiguous || pe.Retryable || !cp.current().Unsafe {
		t.Fatalf("failed rejection checkpoint incorrectly allowed retry: %+v", pe)
	}
}

type blockingMedia struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
	stop    sync.Once
}

func (b *blockingMedia) Open(context.Context, model.Media) (io.ReadCloser, int64, error) {
	return b, 1024, nil
}

func (b *blockingMedia) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, context.Canceled
}

func (b *blockingMedia) Close() error {
	b.stop.Do(func() { close(b.closed) })
	return nil
}

func TestCancellationClosesSourceWithoutPublishing(t *testing.T) {
	media := &blockingMedia{started: make(chan struct{}), closed: make(chan struct{})}
	cp := new(checkpoints)
	var requests atomic.Int32
	c, _ := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/2/media/upload/initialize" {
			t.Errorf("unexpected request after canceled source read: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":{"id":"900","expires_after_secs":86400}}`)
	}, media)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Publish(ctx, fixtureToken(), model.Content{Media: []model.Media{{URL: "https://cdn.example/image.jpg", MIMEType: "image/jpeg", SizeBytes: 1024}}}, model.Progress{}, cp.save)
		done <- err
	}()
	select {
	case <-media.started:
	case <-time.After(time.Second):
		t.Fatal("source read did not begin")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || cp.current().Unsafe || requests.Load() != 1 {
			t.Fatalf("canceled upload result: %v, unsafe=%v, requests=%d", err, cp.current().Unsafe, requests.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock source reading")
	}
}
