// Package handlers — upload intake validation.
//
// Because uploads go straight to S3 via presigned multipart URLs, the API
// never sees the bytes at create time. Validation therefore happens in two
// places: a content-type allow-list gate at Create (cheap, rejects
// obviously-wrong intents early) and a magic-byte sniff of the assembled
// object's header at Complete (authoritative — the client cannot lie about
// the actual bytes). An optional AV scan hook runs at Complete too.
package handlers

import (
	"bytes"
	"strings"
)

// allowedUploadContentTypes is the set of content types accepted at
// upload creation. Anything else is rejected with 415 before an S3
// multipart upload is even started. Kept deliberately tight to audio.
var allowedUploadContentTypes = map[string]struct{}{
	"audio/wav": {}, "audio/x-wav": {}, "audio/wave": {}, "audio/vnd.wave": {},
	"audio/mpeg": {}, "audio/mp3": {},
	"audio/flac": {}, "audio/x-flac": {},
	"audio/ogg": {}, "application/ogg": {}, "audio/opus": {},
	"audio/mp4": {}, "audio/x-m4a": {}, "audio/aac": {},
	"audio/aiff": {}, "audio/x-aiff": {},
	"audio/webm": {},
	"audio/amr":  {},
}

// isAllowedUploadContentType reports whether a declared content type is
// an accepted audio type. Any "audio/..." subtype is allowed (forward
// compatibility), plus the explicit non-"audio/" containers above.
func isAllowedUploadContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 { // strip "; charset=..." etc.
		ct = strings.TrimSpace(ct[:i])
	}
	if strings.HasPrefix(ct, "audio/") {
		return true
	}
	_, ok := allowedUploadContentTypes[ct]
	return ok
}

// detectAudioFormat sniffs the leading bytes of a file and returns a
// short format label (e.g. "wav", "mp3", "flac") plus whether the header
// matches a recognised audio container/codec. It is intentionally
// conservative: an unrecognised header returns ("", false).
func detectAudioFormat(h []byte) (string, bool) {
	if len(h) < 12 {
		return "", false
	}
	switch {
	case bytes.Equal(h[0:4], []byte("RIFF")) && bytes.Equal(h[8:12], []byte("WAVE")):
		return "wav", true
	case bytes.Equal(h[0:4], []byte("fLaC")):
		return "flac", true
	case bytes.Equal(h[0:4], []byte("OggS")):
		return "ogg", true // ogg/opus/vorbis
	case bytes.Equal(h[0:3], []byte("ID3")):
		return "mp3", true // ID3v2-tagged mp3
	case h[0] == 0xFF && (h[1]&0xE0) == 0xE0:
		// MPEG audio frame sync (mp3) or ADTS AAC (0xFFF...).
		if (h[1] & 0xF6) == 0xF0 {
			return "aac", true
		}
		return "mp3", true
	case bytes.Equal(h[4:8], []byte("ftyp")):
		return "mp4", true // m4a/mp4/aac-in-mp4
	case bytes.Equal(h[0:4], []byte("FORM")) && bytes.Equal(h[8:12], []byte("AIFF")):
		return "aiff", true
	case bytes.Equal(h[0:4], []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "webm", true // Matroska/WebM (EBML)
	case len(h) >= 6 && bytes.Equal(h[0:6], []byte("#!AMR\n")):
		return "amr", true
	default:
		return "", false
	}
}
