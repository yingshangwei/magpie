package gateway

import (
	"net/http"
	"testing"
)

func TestPeerSessionsDoNotShareLocalOrOtherPeerAffinity(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"same first message"}]}`)
	for _, headers := range []http.Header{
		{},
		{SessionHeader: {"session"}},
		{SessionHeader: {"displayed"}, "Session_id": {"native"}},
	} {
		a, b, again := headers.Clone(), headers.Clone(), headers.Clone()
		peerSessionHeaders(a, "alpha", body)
		peerSessionHeaders(b, "beta", body)
		peerSessionHeaders(again, "alpha", body)
		if sessionOf(a) == sessionOf(headers) || sessionOf(a) == sessionOf(b) || sessionOf(a) != sessionOf(again) {
			t.Fatal("peer usage sessions collide or change between turns")
		}
		if nativeSessionOf(a) == nativeSessionOf(b) || nativeSessionOf(a) == nativeSessionOf(headers) {
			t.Fatal("peer native sessions collide")
		}
		if conversationID(a, body) == conversationID(b, body) || conversationID(a, body) == conversationID(headers, body) {
			t.Fatal("peer affinity can select another caller's conversation")
		}
		continued := []byte(`{"messages":[{"role":"user","content":"same first message"},{"role":"assistant","content":"answer"},{"role":"user","content":"continue"}]}`)
		next := headers.Clone()
		peerSessionHeaders(next, "alpha", continued)
		if conversationID(a, body) != conversationID(next, continued) {
			t.Fatal("continued conversation lost its peer affinity")
		}
	}
}
