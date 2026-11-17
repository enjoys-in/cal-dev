package airsend

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSendCalendarMailSignsRequest(t *testing.T) {
	const token, secret = "tok-123", "sec-456"
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		ts := r.Header.Get("X-Caldev-Timestamp")
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "."))
		mac.Write(gotBody)
		if want := hex.EncodeToString(mac.Sum(nil)); r.Header.Get("X-Caldev-Signature") != want {
			t.Errorf("signature mismatch")
		}
		if r.Header.Get("X-Caldev-Message-Id") != "m1" {
			t.Errorf("message id header = %q", r.Header.Get("X-Caldev-Message-Id"))
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := New(srv.URL, token, secret, time.Second)
	if err := c.SendCalendarMail(context.Background(), &CalendarMail{Version: 1, MessageID: "m1", Method: "REQUEST"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotBody) == 0 {
		t.Fatal("empty body")
	}
}

func TestSendCalendarMailStatusHandling(t *testing.T) {
	cases := []struct {
		status     int
		retryAfter string
		wantNil    bool
		retryable  bool
		after      time.Duration
	}{
		{http.StatusAccepted, "", true, false, 0},
		{http.StatusConflict, "", true, false, 0}, // already handled → success
		{http.StatusTooManyRequests, "300", false, true, 300 * time.Second},
		{http.StatusServiceUnavailable, "", false, true, 0},
		{http.StatusBadGateway, "", false, true, 0},
		{http.StatusUnauthorized, "", false, false, 0},
		{http.StatusForbidden, "", false, false, 0},
		{http.StatusBadRequest, "", false, false, 0},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.retryAfter != "" {
				w.Header().Set("Retry-After", tc.retryAfter)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"message":"x"}`))
		}))
		err := New(srv.URL, "t", "s", time.Second).SendCalendarMail(context.Background(), &CalendarMail{MessageID: "m"})
		srv.Close()

		if tc.wantNil {
			if err != nil {
				t.Errorf("status %d: want nil, got %v", tc.status, err)
			}
			continue
		}
		var se *SendError
		if !errors.As(err, &se) {
			t.Fatalf("status %d: want *SendError, got %v", tc.status, err)
		}
		if se.Retryable != tc.retryable || se.RetryAfter != tc.after {
			t.Errorf("status %d: retryable=%v after=%v, want %v %v", tc.status, se.Retryable, se.RetryAfter, tc.retryable, tc.after)
		}
	}
}

func TestSendCalendarMailNetworkErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening any more

	err := New(url, "t", "s", time.Second).SendCalendarMail(context.Background(), &CalendarMail{MessageID: "m"})
	var se *SendError
	if !errors.As(err, &se) || !se.Retryable {
		t.Fatalf("want retryable SendError, got %v", err)
	}
}

func TestNewDisabledWithoutURL(t *testing.T) {
	if New("", "t", "s", time.Second) != nil {
		t.Fatal("empty URL must disable the client")
	}
}
