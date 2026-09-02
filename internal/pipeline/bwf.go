package pipeline

// bwf.go patches a Broadcast Wave Format "bext" chunk (EBU Tech 3285) into a
// plain WAV file written by wavenc, carrying the recording's real-world start
// time as a sample-accurate TimeReference. This is the same mechanism
// professional field recorders (Sound Devices, Zoom) use when jam-synced to a
// camera, and what DaVinci Resolve's "Auto Sync Audio using Timecode" reads
// natively — no synthetic video-track workaround needed, unlike the approach
// this replaced (see git history of BuildAudioRecordStr).
//
// wavenc (gst-plugins-good) has no bext-aware property — confirmed via
// `gst-inspect-1.0 wavenc`, it only implements the generic GstTagSetter/
// GstTocSetter interfaces, neither of which produces a bext chunk — so this
// is done as a post-processing pass in Go instead of trying to get GStreamer
// to do it.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"time"
)

// bextBodySize is the fixed-size portion of a version-0 bext chunk (no UMID
// or loudness fields, no CodingHistory): 256 (Description) + 32 (Originator)
// + 32 (OriginatorReference) + 10 (OriginationDate) + 8 (OriginationTime) +
// 4 + 4 (TimeReference low/high) + 2 (Version) + 64 (UMID) + 2*5 (loudness
// fields) + 180 (Reserved) = 602 bytes — the standard fixed bext size
// regardless of version, per EBU Tech 3285. Even, so it never needs RIFF's
// pad-to-even-length byte.
const bextBodySize = 602

// buildBextChunk returns a complete "bext" RIFF chunk (8-byte chunk header +
// bextBodySize-byte body) with TimeReference set to the number of samples
// elapsed, at sampleRate, since UTC midnight of startedAt's day. Version 0:
// UMID and loudness fields are left zeroed (none provided), as is
// CodingHistory (optional, omitted entirely here).
func buildBextChunk(startedAt time.Time, sampleRate int) []byte {
	startedAt = startedAt.UTC()
	midnight := time.Date(startedAt.Year(), startedAt.Month(), startedAt.Day(), 0, 0, 0, 0, time.UTC)
	// float64 has 52 bits of mantissa; even at 192kHz a full day of samples
	// (~1.66e10) is far below 2^53, so this multiplication is exact.
	timeRef := uint64(startedAt.Sub(midnight).Seconds() * float64(sampleRate))

	body := make([]byte, bextBodySize)
	copy(body[0:256], "RaceCast-Emitter microphone recording")
	copy(body[256:288], "RaceCast-Emitter")
	// OriginatorReference [288:320] left zeroed — no house ID scheme in use.
	off := 320
	copy(body[off:off+10], startedAt.Format("2006-01-02"))
	off += 10
	copy(body[off:off+8], startedAt.Format("15:04:05"))
	off += 8
	binary.LittleEndian.PutUint32(body[off:off+4], uint32(timeRef))
	off += 4
	binary.LittleEndian.PutUint32(body[off:off+4], uint32(timeRef>>32))
	// off += 4 lands on Version, left 0 — everything after (UMID, loudness,
	// Reserved) stays zeroed too, all valid for version 0.

	chunk := make([]byte, 8+len(body))
	copy(chunk[0:4], "bext")
	binary.LittleEndian.PutUint32(chunk[4:8], uint32(len(body)))
	copy(chunk[8:], body)
	return chunk
}

// injectBWFTimeReference patches a bext chunk into the WAV file at path,
// immediately after the 12-byte RIFF/WAVE header — the position EBU Tech
// 3285 recommends bext occupy (before fmt), and valid regardless of which
// chunk(s) wavenc happens to write next, since chunk order otherwise doesn't
// matter to a spec-conforming reader.
//
// Rewrites via a temp file + atomic rename rather than loading the file into
// memory: a multi-hour rally recording at 48kHz/16-bit stereo is hundreds of
// MB to several GB, far too large to buffer whole just to patch an 8-byte
// header field and insert one small chunk near the start.
func injectBWFTimeReference(path string, startedAt time.Time, sampleRate int) (err error) {
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer src.Close()

	var header [12]byte
	if _, err := io.ReadFull(src, header[:]); err != nil {
		return fmt.Errorf("read RIFF header: %w", err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return fmt.Errorf("not a RIFF/WAVE file (got %q/%q)", header[0:4], header[8:12])
	}
	origRIFFSize := binary.LittleEndian.Uint32(header[4:8])

	tmpPath := path + ".bwf.tmp"
	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpPath)
		}
	}()

	bext := buildBextChunk(startedAt, sampleRate)

	w := bufio.NewWriter(dst)
	var newHeader [12]byte
	copy(newHeader[0:4], "RIFF")
	binary.LittleEndian.PutUint32(newHeader[4:8], origRIFFSize+uint32(len(bext)))
	copy(newHeader[8:12], "WAVE")
	if _, err = w.Write(newHeader[:]); err != nil {
		dst.Close()
		return fmt.Errorf("write RIFF header: %w", err)
	}
	if _, err = w.Write(bext); err != nil {
		dst.Close()
		return fmt.Errorf("write bext chunk: %w", err)
	}
	if _, err = io.Copy(w, src); err != nil {
		dst.Close()
		return fmt.Errorf("copy audio data: %w", err)
	}
	if err = w.Flush(); err != nil {
		dst.Close()
		return fmt.Errorf("flush: %w", err)
	}
	if err = dst.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	ok = true
	return nil
}
