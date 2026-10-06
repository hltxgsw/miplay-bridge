package miplay

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"testing"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func encodeMediaFrame(payload []byte) []byte {
	frame := []byte{MediaMagic, byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload))}
	return append(frame, payload...)
}

func TestMediaFrameDecoderRoundTrip(t *testing.T) {
	payloads := [][]byte{[]byte("first"), []byte("second-frame")}
	stream := []byte{}
	for _, payload := range payloads {
		stream = append(stream, encodeMediaFrame(payload)...)
	}

	decoder := NewMediaFrameDecoder()
	var frames [][]byte
	for _, chunk := range [][]byte{stream[:5], stream[5:11], stream[11:]} {
		parsed, err := decoder.Feed(chunk)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, parsed...)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	for index, payload := range payloads {
		if !bytes.Equal(frames[index], payload) {
			t.Fatalf("frames[%d] = %q, want %q", index, frames[index], payload)
		}
	}
}

func TestMediaFrameDecoderRejectsCorruption(t *testing.T) {
	decoder := NewMediaFrameDecoder()
	if _, err := decoder.Feed([]byte{0x25, 0, 0, 1, 0xAA}); err == nil || !IsProtocolError(err) {
		t.Fatalf("err = %v, want a media-magic protocol error", err)
	}
	decoder = NewMediaFrameDecoder()
	if _, err := decoder.Feed([]byte{MediaMagic, 0, 0, 0}); err == nil {
		t.Fatal("a zero-length media frame must be rejected")
	}
}

func buildRTPPacket(transportStream []byte) []byte {
	packet := make([]byte, rtpHeaderSize+len(transportStream))
	packet[0] = 0x80
	packet[1] = rtpPayloadTypeMPEGTS | 0x80 // marker bit set
	binary.BigEndian.PutUint16(packet[2:4], 77)
	binary.BigEndian.PutUint32(packet[4:8], 9000)
	binary.BigEndian.PutUint32(packet[8:12], 0xAABBCCDD)
	copy(packet[rtpHeaderSize:], transportStream)
	return packet
}

func buildTransportStream(packets int) []byte {
	transportStream := make([]byte, packets*mpegtsPacketSize)
	for offset := 0; offset < len(transportStream); offset += mpegtsPacketSize {
		transportStream[offset] = 0x47
	}
	return transportStream
}

func TestDecodeRTPMPEGTS(t *testing.T) {
	transportStream := buildTransportStream(2)
	decoded, err := DecodeRTPMPEGTS(buildRTPPacket(transportStream))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Sequence != 77 || decoded.Timestamp != 9000 || decoded.SSRC != 0xAABBCCDD {
		t.Fatalf("decoded = %+v", decoded)
	}
	if !decoded.Marker {
		t.Fatal("marker bit must be decoded")
	}
	if !bytes.Equal(decoded.TransportStream, transportStream) {
		t.Fatal("transport stream payload mismatch")
	}
}

func TestDecodeRTPMPEGTSRejectsCorruption(t *testing.T) {
	transportStream := buildTransportStream(1)

	badFlags := buildRTPPacket(transportStream)
	badFlags[0] = 0x81
	if _, err := DecodeRTPMPEGTS(badFlags); err == nil {
		t.Fatal("an unsupported RTP version must be rejected")
	}

	badType := buildRTPPacket(transportStream)
	badType[1] = 96 // dynamic payload type, not MPEG-TS
	if _, err := DecodeRTPMPEGTS(badType); err == nil {
		t.Fatal("a non-MPEG-TS payload type must be rejected")
	}

	badSync := buildRTPPacket(transportStream)
	badSync[rtpHeaderSize] = 0x46
	if _, err := DecodeRTPMPEGTS(badSync); err == nil {
		t.Fatal("a missing MPEG-TS sync byte must be rejected")
	}

	oddPayload := buildRTPPacket(append(buildTransportStream(1), 0x00))
	if _, err := DecodeRTPMPEGTS(oddPayload); err == nil {
		t.Fatal("a payload that is not a whole number of 188-byte packets must be rejected")
	}

	if _, err := DecodeRTPMPEGTS(make([]byte, 100)); err == nil {
		t.Fatal("a truncated RTP packet must be rejected")
	}
}

func TestPipelineLifecycleWithoutBinaries(t *testing.T) {
	cfg := config.Default()
	cfg.FFmpegPath = "/nonexistent/ffmpeg-for-test"
	cfg.AplayPath = "/nonexistent/aplay-for-test"
	pipeline := NewPipeline(cfg, discardLogger())

	if err := pipeline.Start("test-session"); err == nil {
		t.Fatal("Start must fail when the child binaries are missing")
	}
	if pipeline.Running() {
		t.Fatal("a failed Start must not report a running pipeline")
	}
	if err := pipeline.Write(buildTransportStream(1)); err == nil {
		t.Fatal("Write before Start must fail")
	}
	pipeline.Stop() // must be idempotent on a stopped pipeline
	if stats := pipeline.Stats(); stats.Running || stats.FFmpegAlive || stats.AplayAlive {
		t.Fatalf("stats = %+v", stats)
	}
}

// ---------------------------------------------------------------------------
// MIPLAY_FFMPEG_AF（可选过滤器链）—— 2026-10-05 卡顿排查用的实验开关
//
// 默认不设 = 与上游命令行逐字一致，这是硬要求：这个开关只用来做一次 A/B
// 实验（区分"率差/时钟漂移"与"数据空洞"），绝不能默认改变播放行为。
// ---------------------------------------------------------------------------

func assertArgsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv length %d != %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q\ngot:  %v\nwant: %v",
				i, got[i], want[i], got, want)
		}
	}
}

func TestFFmpegCommandArgsDefaultMatchesUpstream(t *testing.T) {
	cfg := config.Default()
	cfg.MiPlayFFmpegAF = ""
	want := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", "mpegts",
		"-flags", "low_delay",
		"-probesize", "4096",
		"-analyzeduration", "0",
		"-i", "pipe:0",
		"-vn",
		"-acodec", "pcm_s16le",
		"-ar", "48000",
		"-ac", "2",
		"-f", "s16le",
		"pipe:1",
	}
	assertArgsEqual(t, ffmpegCommandArgs(cfg), want)
}

func TestFFmpegCommandArgsInsertFiltersBeforeTheOutput(t *testing.T) {
	cfg := config.Default()
	cfg.MiPlayFFmpegAF = "aresample=async=1:min_hard_comp=0.02"
	args := ffmpegCommandArgs(cfg)

	afIndex, outputIndex := -1, -1
	for i, arg := range args {
		if arg == "-af" && afIndex < 0 {
			afIndex = i
		}
		if arg == "s16le" && i > 0 && args[i-1] == "-f" && outputIndex < 0 {
			outputIndex = i
		}
	}
	if afIndex < 0 {
		t.Fatalf("no -af in %v", args)
	}
	if outputIndex < 0 || afIndex > outputIndex {
		t.Fatalf("-af must precede the output specifiers: %v", args)
	}
	if got := args[afIndex+1]; got != cfg.MiPlayFFmpegAF {
		t.Fatalf("filter chain = %q, want %q", got, cfg.MiPlayFFmpegAF)
	}
	// 去掉这一对参数后，必须与默认逐字一致（即"只多了一个 -af"）。
	stripped := append(append([]string{}, args[:afIndex]...), args[afIndex+2:]...)
	assertArgsEqual(t, stripped, ffmpegCommandArgs(config.Default()))
}
