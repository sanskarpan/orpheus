package avscan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// fakeReader returns fixed bytes regardless of key, capturing the requested n.
type fakeReader struct {
	data    []byte
	lastN   int64
	failErr error
}

func (f *fakeReader) GetObjectRange(_ context.Context, _ string, n int64) ([]byte, error) {
	f.lastN = n
	if f.failErr != nil {
		return nil, f.failErr
	}
	if n < int64(len(f.data)) {
		return f.data[:n], nil
	}
	return f.data, nil
}

// eicarFile is a clean WAV-ish prefix with the EICAR signature embedded in the
// body, mimicking a polyglot that passes the audio-format gate but is malicious.
func eicarFile() []byte {
	b := new(bytes.Buffer)
	b.WriteString("RIFF....WAVEfmt ")
	b.Write(make([]byte, 24)) // filler
	b.WriteString("data")
	b.Write(eicarSignature())
	b.Write(make([]byte, 16))
	return b.Bytes()
}

func TestSignatureScanner_DetectsEICAR(t *testing.T) {
	s := &SignatureScanner{Reader: &fakeReader{data: eicarFile()}}
	err := s.Scan(context.Background(), "any")
	if !errors.Is(err, ErrInfected) {
		t.Fatalf("want ErrInfected, got %v", err)
	}
}

func TestSignatureScanner_PassesClean(t *testing.T) {
	clean := append([]byte("RIFF....WAVE"), make([]byte, 1024)...)
	s := &SignatureScanner{Reader: &fakeReader{data: clean}}
	if err := s.Scan(context.Background(), "any"); err != nil {
		t.Fatalf("clean content should pass, got %v", err)
	}
}

func TestSignatureScanner_DefaultsMaxBytes(t *testing.T) {
	fr := &fakeReader{data: []byte("RIFF")}
	s := &SignatureScanner{Reader: fr}
	_ = s.Scan(context.Background(), "any")
	if fr.lastN != defaultMaxScanBytes {
		t.Fatalf("expected default scan window %d, got %d", defaultMaxScanBytes, fr.lastN)
	}
}

func TestSignatureScanner_ReadErrorPropagates(t *testing.T) {
	s := &SignatureScanner{Reader: &fakeReader{failErr: errors.New("boom")}}
	if err := s.Scan(context.Background(), "any"); err == nil || errors.Is(err, ErrInfected) {
		t.Fatalf("read error should propagate (not ErrInfected), got %v", err)
	}
}

func TestChain_FirstErrorWins(t *testing.T) {
	ch := Chain{
		&SignatureScanner{Reader: &fakeReader{data: []byte("clean")}},
		&SignatureScanner{Reader: &fakeReader{data: eicarFile()}},
	}
	if err := ch.Scan(context.Background(), "any"); !errors.Is(err, ErrInfected) {
		t.Fatalf("chain should surface the infection, got %v", err)
	}
}

func TestChain_EmptyPasses(t *testing.T) {
	if err := (Chain{}).Scan(context.Background(), "any"); err != nil {
		t.Fatalf("empty chain should pass, got %v", err)
	}
}

// fakeClamd is a minimal clamd INSTREAM server: it reads the chunks and
// replies with FOUND if the reassembled payload contains the marker.
func fakeClamd(t *testing.T, marker []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				// Consume the "zINSTREAM\0" command up to the null byte.
				if _, err := r.ReadBytes(0x00); err != nil {
					return
				}
				var payload bytes.Buffer
				for {
					var sz [4]byte
					if _, err := io.ReadFull(r, sz[:]); err != nil {
						return
					}
					n := binary.BigEndian.Uint32(sz[:])
					if n == 0 {
						break
					}
					chunk := make([]byte, n)
					if _, err := io.ReadFull(r, chunk); err != nil {
						return
					}
					payload.Write(chunk)
				}
				if bytes.Contains(payload.Bytes(), marker) {
					_, _ = c.Write([]byte("stream: Test.Signature FOUND\x00"))
				} else {
					_, _ = c.Write([]byte("stream: OK\x00"))
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestClamd_DetectsInfected(t *testing.T) {
	marker := []byte("EVILMARKER")
	addr := fakeClamd(t, marker)
	c := &Clamd{Addr: addr, Reader: &fakeReader{data: append([]byte("RIFF"), marker...)}, Timeout: 5 * time.Second}
	if err := c.Scan(context.Background(), "any"); !errors.Is(err, ErrInfected) {
		t.Fatalf("want ErrInfected, got %v", err)
	}
}

func TestClamd_PassesClean(t *testing.T) {
	addr := fakeClamd(t, []byte("NEVER-PRESENT"))
	c := &Clamd{Addr: addr, Reader: &fakeReader{data: []byte("RIFF....WAVE clean bytes")}, Timeout: 5 * time.Second}
	if err := c.Scan(context.Background(), "any"); err != nil {
		t.Fatalf("clean content should pass, got %v", err)
	}
}

func TestClamd_DialErrorPropagates(t *testing.T) {
	// Nothing listening on this port.
	c := &Clamd{Addr: "127.0.0.1:1", Reader: &fakeReader{data: []byte("x")}, Timeout: time.Second}
	if err := c.Scan(context.Background(), "any"); err == nil || errors.Is(err, ErrInfected) {
		t.Fatalf("dial failure should propagate (not ErrInfected), got %v", err)
	}
}
