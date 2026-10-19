package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The request is what the server's guards accept: DELETE, and no Origin header.
func TestClearSessions_DecodesCounts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/sessions" || r.Header.Get("Origin") != "" {
			t.Errorf("request %s %s origin=%q, want DELETE /v1/sessions with no Origin", r.Method, r.URL, r.Header.Get("Origin"))
		}
		w.Write([]byte(`{"sessions":3,"archivedSessions":40,"bytes":12345}`))
	}))
	defer ts.Close()
	res, err := New(ts.URL).ClearSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 3 || res.ArchivedSessions != 40 || res.Bytes != 12345 || res.ArchiveError != "" {
		t.Fatalf("ClearSessions = %+v", res)
	}
}

// A refusal carries the server's reason, sanitized like every other server string this client
// prints: the endpoint is unauthenticated and this client cannot verify what answered.
func TestClearSessions_403CarriesTheServersReason(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"not bound to loopback only\u001b[2J"}`))
	}))
	defer ts.Close()
	_, err := New(ts.URL).ClearSessions(context.Background())
	if !errors.Is(err, ErrClearRefused) || !strings.Contains(err.Error(), "not bound to loopback only") {
		t.Fatalf("err = %v, want ErrClearRefused with the server's reason", err)
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Fatalf("an escape sequence reached the message: %q", err)
	}
}

// When the disk half failed the server still cleared memory, and says how much: the counts are
// returned with the archive's error, not dropped for it.
func TestClearSessions_500DecodesCountsAndArchiveError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"sessions":3,"archivedSessions":0,"bytes":0,"archiveError":"could not move the archive aside\u001b[2J"}`))
	}))
	defer ts.Close()
	res, err := New(ts.URL).ClearSessions(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want the counts with ArchiveError set", err)
	}
	if res.Sessions != 3 || res.ArchiveError != "could not move the archive aside[2J" {
		t.Fatalf("ClearSessions = %+v, want the counts and the error with its escape removed", res)
	}
}

// A proxy that predates the endpoint answers 405, since GET /v1/sessions exists. That is "upgrade
// it", not a failure of the clear.
func TestClearSessions_405IsErrClearUnsupported(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer ts.Close()
	if _, err := New(ts.URL).ClearSessions(context.Background()); !errors.Is(err, ErrClearUnsupported) {
		t.Fatalf("err = %v, want ErrClearUnsupported", err)
	}
}

// The id is escaped into one path segment, so a pending bucket's id or one holding a slash
// erases that session and no other.
func TestDeleteSession_EscapesTheIDAndDecodesCounts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.EscapedPath() != "/v1/sessions/pending:bob%2Fshell" || r.Header.Get("Origin") != "" {
			t.Errorf("request %s %s origin=%q, want DELETE /v1/sessions/pending:bob%%2Fshell with no Origin",
				r.Method, r.URL.EscapedPath(), r.Header.Get("Origin"))
		}
		w.Write([]byte(`{"sessions":1,"archivedSessions":1,"bytes":4096}`))
	}))
	defer ts.Close()
	res, err := New(ts.URL).DeleteSession(context.Background(), "pending:bob/shell")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 1 || res.ArchivedSessions != 1 || res.Bytes != 4096 {
		t.Fatalf("DeleteSession = %+v", res)
	}
}

// Each status the route answers maps to what the TUI needs to tell apart: gone already, a proxy
// to upgrade, a refusal with its reason, and memory erased with the disk half failed.
func TestDeleteSession_MapsEveryStatus(t *testing.T) {
	for _, tc := range []struct {
		code    int
		body    string
		wantErr error
		check   func(ClearResult, error) bool
	}{
		{http.StatusNotFound, `404 page not found`, ErrSessionGone, nil},
		{http.StatusMethodNotAllowed, ``, ErrDeleteUnsupported, nil},
		{http.StatusForbidden, `{"error":"not bound to loopback only"}`, ErrClearRefused,
			func(_ ClearResult, err error) bool {
				return strings.Contains(err.Error(), "not bound to loopback only")
			}},
		{http.StatusInternalServerError, `{"sessions":1,"archivedSessions":0,"bytes":0,"archiveError":"disk says no"}`, nil,
			func(r ClearResult, err error) bool {
				return err == nil && r.Sessions == 1 && r.ArchiveError == "disk says no"
			}},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			w.Write([]byte(tc.body))
		}))
		res, err := New(ts.URL).DeleteSession(context.Background(), "s1")
		ts.Close()
		if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
			t.Errorf("%d: err = %v, want %v", tc.code, err, tc.wantErr)
		}
		if tc.check != nil && !tc.check(res, err) {
			t.Errorf("%d: DeleteSession = %+v, %v", tc.code, res, err)
		}
	}
}

// A dot-segment id is sent escaped. Left bare, a mux cleans DELETE /v1/sessions/. into a redirect
// to /v1/sessions, and following it would clear every session instead of erasing this one.
func TestDeleteSession_EscapesDotSegments(t *testing.T) {
	for id, want := range map[string]string{".": "/v1/sessions/%2E", "..": "/v1/sessions/%2E%2E"} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.EscapedPath() != want {
				t.Errorf("id %q: request %s %s, want DELETE %s", id, r.Method, r.URL.EscapedPath(), want)
			}
			w.Write([]byte(`{"sessions":1,"archivedSessions":1,"bytes":10}`))
		}))
		res, err := New(ts.URL).DeleteSession(context.Background(), id)
		ts.Close()
		if err != nil {
			t.Fatalf("id %q: %v", id, err)
		}
		if res.Sessions != 1 || res.ArchivedSessions != 1 || res.Bytes != 10 {
			t.Fatalf("id %q: DeleteSession = %+v", id, res)
		}
	}
}

// Neither erase follows a redirect, so it can only reach the route it named: a 307 to
// /v1/sessions would otherwise re-send the DELETE there, and clear everything.
func TestErase_DoesNotFollowRedirects(t *testing.T) {
	for name, erase := range map[string]func(*Client) error{
		"DeleteSession": func(c *Client) error { _, err := c.DeleteSession(context.Background(), "s1"); return err },
		"ClearSessions": func(c *Client) error { _, err := c.ClearSessions(context.Background()); return err },
	} {
		var requests int
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.Header().Set("Location", "/v1/sessions")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		err := erase(New(ts.URL))
		ts.Close()
		if err == nil {
			t.Errorf("%s: err = nil, want an error for a 307", name)
		}
		if requests != 1 {
			t.Errorf("%s: server saw %d requests, want 1 (the redirect not followed)", name, requests)
		}
	}
}
