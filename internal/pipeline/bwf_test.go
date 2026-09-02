package pipeline

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeMinimalWAV writes a tiny but valid RIFF/WAVE file (fmt + data chunks,
// a handful of silent S16LE mono samples at sampleRate) — enough to exercise
// injectBWFTimeReference without needing a real wavenc-produced file.
func writeMinimalWAV(t *testing.T, path string, sampleRate int) {
	t.Helper()
	const numSamples = 8
	dataSize := numSamples * 2 // S16LE mono: 2 bytes/sample

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(4+8+16+8+dataSize)) // "WAVE"+fmt chunk+data chunk header+data
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16)) // PCM fmt chunk size
	binary.Write(&buf, binary.LittleEndian, uint16(1))  // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1))  // mono
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2)) // byte rate
	binary.Write(&buf, binary.LittleEndian, uint16(2))            // block align
	binary.Write(&buf, binary.LittleEndian, uint16(16))           // bits/sample

	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataSize))
	buf.Write(make([]byte, dataSize)) // silence

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writeMinimalWAV: %v", err)
	}
}

// readChunks parses path's RIFF chunk list into name -> body bytes. Fails the
// test on any structural error, since a malformed file from
// injectBWFTimeReference is itself the bug under test.
func readChunks(t *testing.T, path string) map[string][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("not a valid RIFF/WAVE file: %x", data[:min(12, len(data))])
	}
	riffSize := binary.LittleEndian.Uint32(data[4:8])
	if int(riffSize) != len(data)-8 {
		t.Errorf("RIFF size field = %d, want %d (len(data)-8)", riffSize, len(data)-8)
	}

	chunks := make(map[string][]byte)
	off := 12
	for off+8 <= len(data) {
		name := string(data[off : off+4])
		size := binary.LittleEndian.Uint32(data[off+4 : off+8])
		bodyStart := off + 8
		bodyEnd := bodyStart + int(size)
		if bodyEnd > len(data) {
			t.Fatalf("chunk %q size %d overruns file at offset %d", name, size, off)
		}
		chunks[name] = data[bodyStart:bodyEnd]
		off = bodyEnd
		if size%2 == 1 { // RIFF pad byte
			off++
		}
	}
	return chunks
}

func TestInjectBWFTimeReference_ChunkStructureValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.wav")
	writeMinimalWAV(t, path, 48000)

	origData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	origChunks := readChunks(t, path)
	if _, ok := origChunks["data"]; !ok {
		t.Fatal("test fixture itself has no data chunk")
	}

	startedAt := time.Date(2026, 9, 1, 14, 30, 0, 0, time.UTC)
	if err := injectBWFTimeReference(path, startedAt, 48000); err != nil {
		t.Fatalf("injectBWFTimeReference: %v", err)
	}

	chunks := readChunks(t, path)

	bext, ok := chunks["bext"]
	if !ok {
		t.Fatal("no bext chunk after injection")
	}
	if len(bext) != bextBodySize {
		t.Errorf("bext body size = %d, want %d", len(bext), bextBodySize)
	}

	fmtChunk, ok := chunks["fmt "]
	if !ok || len(fmtChunk) == 0 {
		t.Error("fmt chunk missing or empty after injection")
	}
	dataChunk, ok := chunks["data"]
	if !ok {
		t.Fatal("data chunk missing after injection")
	}
	if !bytes.Equal(dataChunk, origChunks["data"]) {
		t.Error("audio data was altered by injection")
	}
	// The RIFF header + inserted bext chunk is the only thing that should
	// change; everything from "fmt " onward must be byte-identical to the
	// original file's own bytes at the same relative position.
	origBody := origData[12:]
	newBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gotBody := newBody[12+8+bextBodySize:]
	if !bytes.Equal(gotBody, origBody) {
		t.Error("bytes after the inserted bext chunk do not match the original file's post-header bytes")
	}
}

func TestBuildBextChunk_TimeReference(t *testing.T) {
	tests := []struct {
		name       string
		startedAt  time.Time
		sampleRate int
		wantRef    uint64
	}{
		{"midnight UTC", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 48000, 0},
		{"one second past midnight", time.Date(2026, 9, 1, 0, 0, 1, 0, time.UTC), 48000, 48000},
		{"noon", time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), 48000, 12 * 3600 * 48000},
		{"non-UTC input normalised to UTC", time.Date(2026, 9, 1, 15, 30, 0, 0, time.FixedZone("CEST", 2*3600)), 44100,
			uint64((13*3600 + 30*60) * 44100)}, // 15:30 +02:00 == 13:30 UTC
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunk := buildBextChunk(tt.startedAt, tt.sampleRate)
			if string(chunk[0:4]) != "bext" {
				t.Fatalf("chunk fourcc = %q, want bext", chunk[0:4])
			}
			size := binary.LittleEndian.Uint32(chunk[4:8])
			if size != bextBodySize {
				t.Fatalf("declared body size = %d, want %d", size, bextBodySize)
			}
			body := chunk[8:]
			low := binary.LittleEndian.Uint32(body[320+10+8 : 320+10+8+4])
			high := binary.LittleEndian.Uint32(body[320+10+8+4 : 320+10+8+8])
			got := uint64(low) | uint64(high)<<32
			if got != tt.wantRef {
				t.Errorf("TimeReference = %d, want %d", got, tt.wantRef)
			}

			gotDate := string(body[320 : 320+10])
			wantDate := tt.startedAt.UTC().Format("2006-01-02")
			if gotDate != wantDate {
				t.Errorf("OriginationDate = %q, want %q", gotDate, wantDate)
			}
		})
	}
}

func TestInjectBWFTimeReference_RejectsNonWAV(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-wav.bin")
	if err := os.WriteFile(path, []byte("not a riff file at all, just junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := injectBWFTimeReference(path, time.Now(), 48000); err == nil {
		t.Error("expected an error for a non-RIFF file, got nil")
	}
	// Must not have clobbered or partially rewritten the original file.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "not a riff file at all, just junk" {
		t.Errorf("original file was modified despite the error: %q", data)
	}
}
