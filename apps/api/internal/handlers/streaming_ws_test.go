package handlers

import (
	"strings"
	"testing"
)

func TestStreamToken_RoundTrip(t *testing.T) {
	sid := "11111111-1111-1111-1111-111111111111"
	org := "22222222-2222-2222-2222-222222222222"
	tok := MintStreamToken(sid, org)

	gotSid, gotOrg, ok := verifyStreamToken(tok)
	if !ok {
		t.Fatal("valid token failed verification")
	}
	if gotSid != sid || gotOrg != org {
		t.Fatalf("round-trip mismatch: sid=%q org=%q", gotSid, gotOrg)
	}
}

func TestStreamToken_Rejects(t *testing.T) {
	tok := MintStreamToken("sid", "org")

	// tampered signature
	bad := tok[:len(tok)-2] + "xy"
	if _, _, ok := verifyStreamToken(bad); ok {
		t.Error("tampered signature accepted")
	}
	// tampered payload (flip a char in the payload segment)
	parts := strings.SplitN(tok, ".", 2)
	if _, _, ok := verifyStreamToken("AAAA." + parts[1]); ok {
		t.Error("tampered payload accepted")
	}
	// garbage
	for _, g := range []string{"", "no-dot", "a.b.c", "...."} {
		if _, _, ok := verifyStreamToken(g); ok {
			t.Errorf("garbage %q accepted", g)
		}
	}
}

func TestTelephonyToken_RoundTripAndTamper(t *testing.T) {
	org := "22222222-2222-2222-2222-222222222222"
	tok := MintTelephonyToken(org)

	gotOrg, ok := verifyTelephonyToken(tok)
	if !ok || gotOrg != org {
		t.Fatalf("valid telephony token failed: org=%q ok=%v", gotOrg, ok)
	}
	// Tampered signature is rejected.
	if _, ok := verifyTelephonyToken(tok + "x"); ok {
		t.Fatal("tampered telephony token accepted")
	}
	// A stream token must NOT verify as a telephony token (different payload prefix).
	streamTok := MintStreamToken("sid", org)
	if _, ok := verifyTelephonyToken(streamTok); ok {
		t.Fatal("stream token wrongly accepted as telephony token")
	}
}

func TestSwapWSPath(t *testing.T) {
	got := swapWSPath("ws://127.0.0.1:8082/v1/stream/transcribe", "/v1/stream/converse")
	if got != "ws://127.0.0.1:8082/v1/stream/converse" {
		t.Fatalf("swapWSPath = %q", got)
	}
}
