package outis

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

const eventBody = `{"id":"evt_1","type":"request.authorized","created_at":"2026-09-27T12:00:00Z","org":"org_1",` +
	`"data":{"request":{"id":"req-1","action":"deploy.production","requester":"keith","state":"succeeded","live":false,` +
	`"outcome":"authorized","approvers":["maya","sam"],"params":{"env":"production"},"operation_hash":"sha256:x",` +
	`"created_at":1788350100000,"decided_at":1788350400000}}}`

func TestVerifyWebhook(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	opts := &VerifyOptions{Now: func() time.Time { return now }}
	header := func(v string) http.Header {
		h := http.Header{}
		h.Set(SignatureHeader, v)
		return h
	}
	var vErr *WebhookVerificationError

	t.Run("good", func(t *testing.T) {
		ev, err := VerifyWebhook([]byte(eventBody), header(SignWebhook([]byte(eventBody), "whsec_a", now)), "whsec_a", opts)
		if err != nil {
			t.Fatal(err)
		}
		if ev.ID != "evt_1" || ev.Type != EventRequestAuthorized || ev.Org != "org_1" || !ev.Data.Request.IsAuthorized() {
			t.Errorf("event = %+v", ev)
		}
		if !ev.CreatedAt.Equal(time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("CreatedAt = %v", ev.CreatedAt)
		}
	})

	t.Run("stale", func(t *testing.T) {
		sig := SignWebhook([]byte(eventBody), "whsec_a", now.Add(-6*time.Minute))
		if _, err := VerifyWebhook([]byte(eventBody), header(sig), "whsec_a", opts); !errors.As(err, &vErr) {
			t.Fatalf("stale delivery accepted: %v", err)
		}
		sig = SignWebhook([]byte(eventBody), "whsec_a", now.Add(6*time.Minute))
		if _, err := VerifyWebhook([]byte(eventBody), header(sig), "whsec_a", opts); !errors.As(err, &vErr) {
			t.Fatalf("future delivery accepted: %v", err)
		}
		wide := &VerifyOptions{Now: opts.Now, Tolerance: 10 * time.Minute}
		if _, err := VerifyWebhook([]byte(eventBody), header(sig), "whsec_a", wide); err != nil {
			t.Fatalf("delivery inside a wider tolerance refused: %v", err)
		}
	})

	t.Run("tampered", func(t *testing.T) {
		sig := SignWebhook([]byte(eventBody), "whsec_a", now)
		tampered := strings.Replace(eventBody, "request.authorized", "request.denied", 1)
		if _, err := VerifyWebhook([]byte(tampered), header(sig), "whsec_a", opts); !errors.As(err, &vErr) {
			t.Fatalf("tampered body accepted: %v", err)
		}
		if _, err := VerifyWebhook([]byte(eventBody), header(sig), "whsec_b", opts); !errors.As(err, &vErr) {
			t.Fatalf("wrong secret accepted: %v", err)
		}
	})

	t.Run("rotated", func(t *testing.T) {
		oldSig := SignWebhook([]byte(eventBody), "whsec_old", now)
		newSig := SignWebhook([]byte(eventBody), "whsec_new", now)
		both := oldSig + ",v1=" + strings.SplitN(newSig, "v1=", 2)[1]
		for _, secret := range []string{"whsec_old", "whsec_new"} {
			if _, err := VerifyWebhook([]byte(eventBody), header(both), secret, opts); err != nil {
				t.Errorf("%s refused during rotation: %v", secret, err)
			}
		}
	})

	t.Run("malformed", func(t *testing.T) {
		for _, h := range []http.Header{{}, header("v1=abcd"), header("t=1800000000"), header("garbage")} {
			if _, err := VerifyWebhook([]byte(eventBody), h, "whsec_a", opts); !errors.As(err, &vErr) {
				t.Errorf("header %v accepted: %v", h, err)
			}
		}
		if _, err := VerifyWebhook([]byte(eventBody), header(SignWebhook([]byte(eventBody), "", now)), "", opts); !errors.As(err, &vErr) {
			t.Error("empty secret accepted")
		}
	})
}
