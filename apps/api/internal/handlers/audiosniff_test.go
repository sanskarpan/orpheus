package handlers

import "testing"

func TestIsAllowedUploadContentType(t *testing.T) {
	allowed := []string{"audio/wav", "audio/mpeg", "audio/flac", "audio/ogg", "audio/mp4", "audio/x-m4a", "audio/webm", "audio/aac", "application/ogg", "audio/anything-new", "audio/wav; charset=binary"}
	for _, ct := range allowed {
		if !isAllowedUploadContentType(ct) {
			t.Errorf("isAllowedUploadContentType(%q) = false, want true", ct)
		}
	}
	rejected := []string{"", "text/html", "image/png", "application/json", "application/octet-stream", "video/mp4", "text/plain"}
	for _, ct := range rejected {
		if isAllowedUploadContentType(ct) {
			t.Errorf("isAllowedUploadContentType(%q) = true, want false", ct)
		}
	}
}

func TestDetectAudioFormat(t *testing.T) {
	cases := []struct {
		name   string
		header []byte
		want   string
		ok     bool
	}{
		{"wav", append([]byte("RIFF\x24\x08\x00\x00WAVE"), make([]byte, 8)...), "wav", true},
		{"flac", append([]byte("fLaC"), make([]byte, 12)...), "flac", true},
		{"ogg", append([]byte("OggS"), make([]byte, 12)...), "ogg", true},
		{"mp3-id3", append([]byte("ID3\x04\x00"), make([]byte, 12)...), "mp3", true},
		{"mp3-sync", append([]byte{0xFF, 0xFB, 0x90, 0x00}, make([]byte, 12)...), "mp3", true},
		{"aac-adts", append([]byte{0xFF, 0xF1, 0x00, 0x00}, make([]byte, 12)...), "aac", true},
		{"mp4-ftyp", append([]byte("\x00\x00\x00\x18ftypM4A "), make([]byte, 8)...), "mp4", true},
		{"aiff", append([]byte("FORM\x00\x00\x00\x00AIFF"), make([]byte, 8)...), "aiff", true},
		{"webm", append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 12)...), "webm", true},
		{"html", []byte("<!DOCTYPE html><html>"), "", false},
		{"zip", append([]byte("PK\x03\x04"), make([]byte, 12)...), "", false},
		{"too-short", []byte("RIFF"), "", false},
		{"empty", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := detectAudioFormat(tc.header)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("detectAudioFormat(%s) = (%q,%v), want (%q,%v)", tc.name, got, ok, tc.want, tc.ok)
			}
		})
	}
}
