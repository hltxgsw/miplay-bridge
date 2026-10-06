package miplay

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
)

// Wire media framing constants.
const (
	// MediaMagic is the first byte of every media frame (same 0x24 marker).
	MediaMagic = 0x24
	// MediaHeaderSize is the media frame header: magic (1) + length (3).
	MediaHeaderSize = 4
	// MaxMediaPayload bounds one media frame.
	MaxMediaPayload = 0xFFFFFF

	rtpHeaderSize        = 12
	rtpPayloadTypeMPEGTS = 33
	mpegtsPacketSize     = 188
	maxTSPacketsPerRTP   = 7
)

// MediaFrameDecoder incrementally splits the media TCP stream into payloads
// framed as '0x24' + three-byte big-endian length.
type MediaFrameDecoder struct {
	buffer []byte
}

// NewMediaFrameDecoder returns an empty decoder.
func NewMediaFrameDecoder() *MediaFrameDecoder {
	return &MediaFrameDecoder{buffer: make([]byte, 0, 64*1024)}
}

// Feed appends data and returns every complete media frame.
func (d *MediaFrameDecoder) Feed(data []byte) ([][]byte, error) {
	d.buffer = append(d.buffer, data...)
	frames := [][]byte{}
	for len(d.buffer) >= MediaHeaderSize {
		if source := d.buffer[0]; source != MediaMagic {
			d.buffer = d.buffer[:0]
			return nil, protocolErrorf("invalid media-frame magic 0x%02x", source)
		}
		length := int(d.buffer[1])<<16 | int(d.buffer[2])<<8 | int(d.buffer[3])
		if length == 0 || length > MaxMediaPayload {
			d.buffer = d.buffer[:0]
			return nil, protocolErrorf("media payload of %d bytes exceeds the safety limit", length)
		}
		frameLength := MediaHeaderSize + length
		if len(d.buffer) < frameLength {
			break
		}
		frame := make([]byte, length)
		copy(frame, d.buffer[MediaHeaderSize:frameLength])
		d.buffer = append(d.buffer[:0], d.buffer[frameLength:]...)
		frames = append(frames, frame)
	}
	return frames, nil
}

// RTPPacket is one validated RTP packet carrying MPEG-TS.
type RTPPacket struct {
	Sequence        uint16
	Timestamp       uint32
	SSRC            uint32
	Marker          bool
	TransportStream []byte
}

// DecodeRTPMPEGTS validates and decodes an RTP packet whose payload must be a
// whole number of 188-byte MPEG-TS packets with intact sync bytes.
func DecodeRTPMPEGTS(data []byte) (RTPPacket, error) {
	if len(data) < rtpHeaderSize+mpegtsPacketSize {
		return RTPPacket{}, protocolErrorf("RTP packet is truncated")
	}
	if data[0] != 0x80 {
		return RTPPacket{}, protocolErrorf("unsupported RTP header flags 0x%02x", data[0])
	}
	payloadType := data[1] & 0x7F
	if payloadType != rtpPayloadTypeMPEGTS {
		return RTPPacket{}, protocolErrorf("unsupported RTP payload type %d", payloadType)
	}
	transportStream := data[rtpHeaderSize:]
	if err := validateTransportStream(transportStream); err != nil {
		return RTPPacket{}, err
	}
	sequence := binary.BigEndian.Uint16(data[2:4])
	timestamp := binary.BigEndian.Uint32(data[4:8])
	ssrc := binary.BigEndian.Uint32(data[8:12])
	return RTPPacket{
		Sequence:        sequence,
		Timestamp:       timestamp,
		SSRC:            ssrc,
		Marker:          data[1]&0x80 != 0,
		TransportStream: transportStream,
	}, nil
}

func validateTransportStream(payload []byte) error {
	if len(payload) == 0 || len(payload)%mpegtsPacketSize != 0 {
		return protocolErrorf("MPEG-TS payload must contain complete 188-byte packets")
	}
	for offset := 0; offset < len(payload); offset += mpegtsPacketSize {
		if payload[offset] != 0x47 {
			return protocolErrorf("MPEG-TS sync byte is missing at offset %d", offset)
		}
	}
	return nil
}

// PipelineStats is the realtime pipeline snapshot for /api/status.
type PipelineStats struct {
	Running      bool      `json:"running"`
	Frames       int64     `json:"frames"`
	Bytes        int64     `json:"bytes"`
	FirstFrameAt time.Time `json:"first_frame_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	FFmpegAlive  bool      `json:"ffmpeg_alive"`
	AplayAlive   bool      `json:"aplay_alive"`
}

// lineRing keeps the last log lines of a child process.
type lineRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newLineRing(max int) *lineRing { return &lineRing{max: max, lines: make([]string, 0, max)} }

func (r *lineRing) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *lineRing) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, " | ")
}

// logPipe forwards a child process stream into the ring buffer and the
// container log.
type logPipe struct {
	ring   *lineRing
	logger *slog.Logger
	prefix string
	buffer []byte
}

func (p *logPipe) Write(data []byte) (int, error) {
	p.buffer = append(p.buffer, data...)
	for {
		index := bytes.IndexByte(p.buffer, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimRight(string(p.buffer[:index]), "\r")
		p.buffer = p.buffer[index+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		p.ring.add(line)
		p.logger.Warn(p.prefix, "line", line)
	}
	if len(p.buffer) > 8*1024 {
		p.buffer = p.buffer[len(p.buffer)-4096:]
	}
	return len(data), nil
}

// Pipeline is the realtime playback path of MiPlay audio:
//
//	media frames -> ffmpeg (MPEG-TS/AAC -> s16le) -> aplay (exclusive hw)
//
// ffmpeg runs with low-latency input flags; aplay owns an ALSA buffer sized
// by MIPLAY_ALSA_BUFFER_US, the primary latency knob.
type Pipeline struct {
	cfg    config.Config
	logger *slog.Logger

	// lifecycle serialises whole Start/Stop cycles. Stop() releases p.mu before
	// waiting for aplay to be reaped (it can take up to the grace period), so
	// without this a Start() issued by a newer session could slip in mid-teardown
	// and leave two aplay processes fighting for the same hw device -- exactly the
	// underrun + "Resource busy" pair seen on the live box.
	lifecycle sync.Mutex

	mu sync.Mutex
	// owner is the session token that started the current pipeline. It keeps a
	// superseded session from tearing down the pipeline that replaced it.
	owner string
	// retiredOwner is the token whose pipeline was stopped last. A session that
	// has been torn down must never bring the pipeline back up: its read loop can
	// be parked mid-frame, and a frame delivered after the stop used to start
	// ffmpeg/aplay again for a session that no longer exists. Nothing fed that
	// pair, so it kept hw:0,0 open until the container was restarted -- the
	// 2026-10-05 outage, where aplay 508 / ffmpeg 509 outlived the cast by
	// minutes and every later cast was silent.
	retiredOwner string

	ffmpeg *exec.Cmd
	aplay  *exec.Cmd
	stdin  io.WriteCloser

	stats    PipelineStats
	ffmpegEP *lineRing
	aplayEP  *lineRing
}

// errPipelineBusy guards the "one writer per device" invariant at the lowest
// level: a session must never adopt or start over a live pipeline it does not own.
var errPipelineBusy = errors.New("the realtime pipeline is owned by another session")

// errSessionOver marks the point where a read loop discovers that its session is
// no longer the current cast. It is the normal way for a superseded session to
// end, not a failure, so callers unwind quietly instead of warning per frame.
var errSessionOver = errors.New("the session is no longer the current cast")

// NewPipeline creates the pipeline for the configured ALSA device.
func NewPipeline(cfg config.Config, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pipeline{
		cfg:      cfg,
		logger:   logger,
		ffmpegEP: newLineRing(30),
		aplayEP:  newLineRing(30),
	}
}

// ffmpegCommandArgs is the realtime decoder command line.
//
// It is a pure function so that "unset MIPLAY_FFMPEG_AF leaves the upstream argv
// untouched" can be asserted in a test: this knob exists to run one A/B
// experiment, not to change playback by accident.
//
// On resampling: forcing -ar 48000 already makes ffmpeg insert a resampler (the
// phone usually streams 44.1 kHz AAC), but an implicit resampler has async
// compensation OFF. Without it, any difference between the phone's sample clock
// and the ALSA clock accumulates in the aplay ring until it underruns.
func ffmpegCommandArgs(cfg config.Config) []string {
	args := []string{
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
	}
	// -af is an output option, so it has to come before the output specifiers.
	if filters := strings.TrimSpace(cfg.MiPlayFFmpegAF); filters != "" {
		args = append(args, "-af", filters)
	}
	return append(args, "-f", "s16le", "pipe:1")
}

// Start launches ffmpeg and aplay on behalf of owner. Starting twice for the
// same owner is a no-op; a session that does not own a live pipeline must not
// adopt it (that would mean two writers on one exclusive device), and a session
// whose pipeline was already stopped is retired for good.
func (p *Pipeline) Start(owner string) error {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	return p.startLocked(owner)
}

// Reclaim makes owner the writer of the pipeline, collecting a leftover one
// first, and reports the token it replaced ("" when there was nothing to
// replace).
//
// It exists for the session that holds the card. The engine has already said the
// device is ours, so a pipeline still running under a session that is gone has
// to be collected rather than reported as busy: refusing there is what left the
// live box completely silent while a zombie aplay held hw:0,0. Reclaim also
// restarts the pipeline when an earlier teardown stopped it, which is how a
// fresh cast heals the leftover without a container restart.
func (p *Pipeline) Reclaim(owner string) (string, error) {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()

	p.mu.Lock()
	running := p.ffmpeg != nil || p.aplay != nil
	current := p.owner
	p.mu.Unlock()

	replaced := ""
	if running && current != owner {
		replaced = current
		if replaced == "" {
			replaced = "unknown"
		}
		// stopLocked reaps both children, so the device is genuinely free before
		// the replacement opens it.
		p.stopLocked()
	}
	if err := p.startLocked(owner); err != nil {
		return replaced, err
	}
	return replaced, nil
}

// startLocked is the shared body of Start and Reclaim; callers hold p.lifecycle.
func (p *Pipeline) startLocked(owner string) error {
	p.mu.Lock()
	if owner != "" && owner == p.retiredOwner {
		retired := p.retiredOwner
		p.mu.Unlock()
		return fmt.Errorf("%w (retired owner %s)", errSessionOver, retired)
	}
	if p.ffmpeg != nil || p.aplay != nil {
		current := p.owner
		p.mu.Unlock()
		if current == owner {
			return nil
		}
		return fmt.Errorf("%w (held by %s)", errPipelineBusy, current)
	}
	p.mu.Unlock()

	aplay := exec.Command(p.cfg.AplayPath,
		"-D", p.cfg.ALSAHardwareDevice(),
		"-t", "raw",
		"-f", "S16_LE",
		"-r", "48000",
		"-c", "2",
		"-B", fmt.Sprintf("%d", p.cfg.MiPlayAlsaBufferUS),
		"-q",
	)
	aplay.Stderr = &logPipe{ring: p.aplayEP, logger: p.logger, prefix: "aplay"}

	ffmpeg := exec.Command(p.cfg.FFmpegPath, ffmpegCommandArgs(p.cfg)...)
	ffmpeg.Stderr = &logPipe{ring: p.ffmpegEP, logger: p.logger, prefix: "ffmpeg"}

	decoded, err := ffmpeg.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open ffmpeg stdout: %w", err)
	}
	aplay.Stdin = decoded
	stdin, err := ffmpeg.StdinPipe()
	if err != nil {
		return fmt.Errorf("open ffmpeg stdin: %w", err)
	}

	if err := aplay.Start(); err != nil {
		return fmt.Errorf("start %s on %s: %w (aplay log: %s)", p.cfg.AplayPath, p.cfg.ALSAHardwareDevice(), err, p.aplayEP.String())
	}
	if err := ffmpeg.Start(); err != nil {
		_ = aplay.Process.Kill()
		_, _ = aplay.Process.Wait()
		return fmt.Errorf("start %s: %w", p.cfg.FFmpegPath, err)
	}

	p.mu.Lock()
	p.aplay = aplay
	p.ffmpeg = ffmpeg
	p.stdin = stdin
	p.owner = owner
	// A new owner means the pipeline is live again on purpose; the previous
	// session's retirement no longer applies to anyone.
	p.retiredOwner = ""
	p.stats = PipelineStats{Running: true, FFmpegAlive: true, AplayAlive: true}
	p.mu.Unlock()
	p.logger.Info("miplay realtime pipeline started",
		"device", p.cfg.ALSAHardwareDevice(),
		"aplay_buffer_us", p.cfg.MiPlayAlsaBufferUS,
		"ffmpeg_pid", ffmpeg.Process.Pid,
		"aplay_pid", aplay.Process.Pid,
		"owner", owner,
	)
	return nil
}

// Write feeds one validated MPEG-TS payload into ffmpeg.
func (p *Pipeline) Write(transportStream []byte) error {
	p.mu.Lock()
	ffmpeg := p.ffmpeg
	stdin := p.stdin
	if ffmpeg == nil || stdin == nil {
		p.mu.Unlock()
		return protocolErrorf("pipeline is not running")
	}
	if p.stats.FirstFrameAt.IsZero() {
		p.stats.FirstFrameAt = time.Now().UTC()
	}
	p.stats.Frames++
	p.stats.Bytes += int64(len(transportStream))
	p.mu.Unlock()

	if _, err := stdin.Write(transportStream); err != nil {
		p.mu.Lock()
		p.stats.LastError = fmt.Sprintf("ffmpeg stdin: %v (ffmpeg log: %s)", err, p.ffmpegEP.String())
		p.mu.Unlock()
		return fmt.Errorf("write to ffmpeg: %w", err)
	}
	return nil
}

// StopOwned releases the pipeline only when owner is the session that started
// it. A session that never owned the pipeline (for example one that was turned
// away because another cast held the card) must not tear down someone else's
// audio -- that was how a stray session used to silence the live one.
func (p *Pipeline) StopOwned(owner string) {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.mu.Lock()
	current := p.owner
	p.mu.Unlock()
	if current == "" || current != owner {
		return
	}
	p.stopLocked()
}

// Stop force-releases the pipeline regardless of owner. Used by the operator
// console and by shutdown, where "stop everything" is the intent.
func (p *Pipeline) Stop() {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.stopLocked()
}

// stopLocked releases the pipeline: closing ffmpeg's stdin lets both processes
// drain and exit; a bounded grace period is followed by SIGKILL. It returns
// only once both children have been reaped, so the ALSA device is really free
// when the caller starts a replacement. Callers must hold p.lifecycle.
func (p *Pipeline) stopLocked() {
	p.mu.Lock()
	ffmpeg := p.ffmpeg
	aplay := p.aplay
	stdin := p.stdin
	p.ffmpeg = nil
	p.aplay = nil
	p.stdin = nil
	// Whoever owned this pipeline is retired now: its read loop may still be
	// holding a frame, and that frame must not start the pipeline again. A forced
	// Stop on an already-idle pipeline must not clear that record, or the very
	// next frame from the dead session brings the writer back.
	if previous := p.owner; previous != "" {
		p.retiredOwner = previous
	}
	p.owner = ""
	running := ffmpeg != nil || aplay != nil
	p.stats.Running = false
	p.stats.FFmpegAlive = false
	p.stats.AplayAlive = false
	p.mu.Unlock()

	if !running {
		return
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	waitWithGrace(ffmpeg, 3*time.Second)
	waitWithGrace(aplay, 3*time.Second)
	p.logger.Info("miplay realtime pipeline stopped",
		"frames", p.stats.Frames, "bytes", p.stats.Bytes,
		"ffmpeg_log_tail", p.ffmpegEP.String(), "aplay_log_tail", p.aplayEP.String())
}

func waitWithGrace(command *exec.Cmd, grace time.Duration) {
	if command == nil || command.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		_ = command.Process.Kill()
		<-done
	}
}

// Running reports whether both child processes are live.
func (p *Pipeline) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ffmpeg != nil
}

// currentOwner reports the token that started the live pipeline, or "" when it
// is idle. Diagnostics use it to answer "whose pipeline is this".
func (p *Pipeline) currentOwner() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ffmpeg == nil && p.aplay == nil {
		return ""
	}
	return p.owner
}

// Stats returns the pipeline snapshot.
func (p *Pipeline) Stats() PipelineStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.stats
	if p.ffmpeg != nil && p.ffmpeg.Process != nil && p.ffmpeg.ProcessState == nil {
		stats.FFmpegAlive = true
	}
	if p.aplay != nil && p.aplay.Process != nil && p.aplay.ProcessState == nil {
		stats.AplayAlive = true
	}
	return stats
}
