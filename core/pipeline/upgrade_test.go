package pipeline

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The header Codex actually sends, and the shape every other WebSocket client sends.
func TestRecordStrippedUpgrade_RecordsTheProtocol(t *testing.T) {
	c := &Context{}
	c.RecordStrippedUpgrade(http.Header{"Upgrade": []string{"websocket"}})
	if c.UpgradeStripped != "websocket" {
		t.Errorf("UpgradeStripped = %q, want websocket", c.UpgradeStripped)
	}
}

// The common case by a wide margin: no Upgrade, so nothing recorded. Pinned because
// the field is omitempty and a non-empty value here would put a key on every row.
func TestRecordStrippedUpgrade_AbsentHeaderRecordsNothing(t *testing.T) {
	for name, h := range map[string]http.Header{
		"no header":     {},
		"empty value":   {"Upgrade": []string{""}},
		"only spaces":   {"Upgrade": []string{"   "}},
		"nil-ish value": {"Upgrade": nil},
	} {
		c := &Context{}
		c.RecordStrippedUpgrade(h)
		if c.UpgradeStripped != "" {
			t.Errorf("%s: UpgradeStripped = %q, want empty", name, c.UpgradeStripped)
		}
	}
}

// Upgrade is client-controlled and the value lands in a JSON event an operator reads
// in a terminal, so it goes through the same sanitiser a User-Agent does. Without it
// a "protocol name" of escape sequences would reach a terminal intact.
func TestRecordStrippedUpgrade_SanitisesControlRunes(t *testing.T) {
	c := &Context{}
	c.RecordStrippedUpgrade(http.Header{"Upgrade": []string{"web\x1b[2Jsocket\x07"}})
	if strings.ContainsAny(c.UpgradeStripped, "\x1b\x07") {
		t.Errorf("UpgradeStripped = %q, still carries control runes", c.UpgradeStripped)
	}
	if c.UpgradeStripped == "" {
		t.Error("sanitising emptied the value; it should neutralise, not discard")
	}
}

// A tab is legal RWS per RFC 9110 but is also a C0 control, so it has to be
// normalised to a space BEFORE the sanitiser turns it into U+FFFD — the same
// ordering ParseUserAgent needs, and for the same reason.
func TestRecordStrippedUpgrade_NormalisesTabBeforeSanitising(t *testing.T) {
	c := &Context{}
	c.RecordStrippedUpgrade(http.Header{"Upgrade": []string{"HTTP/2.0,\twebsocket"}})
	if strings.Contains(c.UpgradeStripped, "�") {
		t.Errorf("UpgradeStripped = %q, tab became a replacement rune instead of a space", c.UpgradeStripped)
	}
	if !strings.Contains(c.UpgradeStripped, "websocket") {
		t.Errorf("UpgradeStripped = %q, want the whole list preserved", c.UpgradeStripped)
	}
}

// Capped, because the header is client-controlled and the field is rendered.
func TestRecordStrippedUpgrade_Capped(t *testing.T) {
	c := &Context{}
	c.RecordStrippedUpgrade(http.Header{"Upgrade": []string{strings.Repeat("w", 4096)}})
	if len(c.UpgradeStripped) > maxClientLen {
		t.Errorf("len = %d, want <= %d", len(c.UpgradeStripped), maxClientLen)
	}
}

// RFC 9110 allows a list, and which element the origin would have chosen is not
// ours to guess — so the whole value is kept rather than the first token.
func TestRecordStrippedUpgrade_KeepsTheWholeList(t *testing.T) {
	c := &Context{}
	c.RecordStrippedUpgrade(http.Header{"Upgrade": []string{"HTTP/2.0, websocket"}})
	if c.UpgradeStripped != "HTTP/2.0, websocket" {
		t.Errorf("UpgradeStripped = %q, want the list intact", c.UpgradeStripped)
	}
}

// The field has to survive the wire, and SessionEvent has a custom MarshalJSON with
// a hand-written wire struct — so a new field is dropped silently unless it is added
// in three places. This is the test that catches two of them.
func TestSessionEvent_UpgradeStrippedRoundTrips(t *testing.T) {
	b, err := json.Marshal(SessionEvent{UpgradeStripped: "websocket"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"upgradeStripped":"websocket"`) {
		t.Fatalf("MarshalJSON dropped the field: %s", b)
	}
	var got SessionEvent
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.UpgradeStripped != "websocket" {
		t.Errorf("after round-trip = %q, want websocket", got.UpgradeStripped)
	}
}

// omitempty: absent on every request that asked for no upgrade, which is nearly all
// of them. A key on every row would be a visible cost for a rare diagnostic.
func TestSessionEvent_UpgradeStrippedOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(SessionEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "upgradeStripped") {
		t.Errorf("empty value emitted a key: %s", b)
	}
}
