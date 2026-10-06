// Package output is the sound-card arbitration engine.
//
// One exclusive ALSA device (no dmix) is shared by three writers: the MiPlay
// realtime pipeline (ffmpeg|aplay), the MPD playback kernel serving DLNA and
// the HTTP API, and the operator console. The engine tracks which source
// currently owns the card, serialises the hand-offs and exposes one typed
// playback surface so no caller has to talk to MPD or amixer directly.
package output

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/alsa"
	"github.com/miplay-bridge/miplay-bridge/internal/config"
	"github.com/miplay-bridge/miplay-bridge/internal/mpd"
)

// Source identifies who owns the sound card right now.
type Source string

// The arbitration states. "api" covers /api/play and the test tone, "dlna"
// covers every URL loaded by an UPnP controller.
const (
	SourceIdle   Source = "idle"
	SourceMiPlay Source = "miplay"
	SourceDLNA   Source = "dlna"
	SourceAPI    Source = "api"
)

// ErrSourceBusy is returned when a claim collides with the current owner.
// The HTTP layer maps it onto 409 device_busy, the DLNA layer onto a UPnP
// "transition not available" fault.
var ErrSourceBusy = errors.New("the sound card is held by another source")

// Holder is one claimant of the exclusive sound card.
//
// The ALSA device has no software mixing, so exactly one writer may exist at a
// time. Holder carries two identities on purpose, because they answer two
// different questions that both used to go wrong on the live box:
//
//   - Peer answers "is this the same sender coming back?" -- a phone
//     reconnecting must not be treated as a newcomer and evicted.
//   - Token answers "is this still the same session?" -- a session that was
//     superseded unwinds later and must not free the slot its successor holds.
type Holder struct {
	Source Source
	// Peer identifies the sender (for MiPlay, the phone address).
	Peer string
	// Token identifies this session instance.
	Token string
	// Evict releases the card on behalf of this holder. It MUST return only
	// after the device is genuinely free (child processes reaped), otherwise
	// the successor races into "Resource busy". A nil Evict means "stop the
	// playback kernel", which is what the DLNA source needs.
	Evict func(ctx context.Context) error
}

func (h Holder) peer() string {
	if h.Peer != "" {
		return h.Peer
	}
	return string(h.Source)
}

func (h Holder) token() string {
	if h.Token != "" {
		return h.Token
	}
	return string(h.Source)
}

// Acquire takes the sound card for h, preempting whoever holds it.
//
// Hand-offs are serialised and the eviction happens with the arbitration lock
// released (it can take seconds), so a newcomer waits for the previous writer
// to actually let go instead of colliding with it.
func (e *Engine) Acquire(ctx context.Context, h Holder) error {
	if h.Source == SourceIdle {
		return errors.New("the idle source cannot claim the sound card")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	e.handoff.Lock()
	defer e.handoff.Unlock()

	e.mu.Lock()
	victimSource, victimPeer, victimToken, victimEvict := e.active, e.holderPeer, e.holderToken, e.evict
	e.mu.Unlock()

	samePeer := victimSource != SourceIdle && victimSource == h.Source && victimPeer == h.peer()
	if victimSource == SourceIdle || samePeer {
		// Free, or the same sender re-claiming: adopt/refresh without eviction.
		e.mu.Lock()
		e.setHolderLocked(h)
		e.mu.Unlock()
		return nil
	}

	if err := e.evictHolder(ctx, victimSource, victimEvict); err != nil {
		return err
	}

	e.mu.Lock()
	// Only drop the slot if the holder is still the one we just evicted; its own
	// Release may already have cleared it, and something else must not be wiped.
	if e.active == victimSource && e.holderToken == victimToken {
		e.active = SourceIdle
		e.holderPeer = ""
		e.holderToken = ""
		e.evict = nil
	}
	e.setHolderLocked(h)
	e.mu.Unlock()
	return nil
}

// evictHolder stops the current writer and waits for the device to be free.
func (e *Engine) evictHolder(ctx context.Context, source Source, evict func(context.Context) error) error {
	e.logger.Info("preempting the current sound card holder", "source", source)
	stop := evict
	if stop == nil {
		stop = e.stopKernelForHandoff
	}
	if err := stop(ctx); err != nil {
		return fmt.Errorf("could not release the sound card from %s: %w", source, err)
	}
	return nil
}

// stopKernelForHandoff halts the playback kernel and waits until it reports a
// non-playing state, so the ALSA device is released before a hand-off.
func (e *Engine) stopKernelForHandoff(ctx context.Context) error {
	if e.client == nil {
		return nil
	}
	if err := e.client.Stop(ctx); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, err := e.kernelState()
		if err != nil {
			// An unreachable kernel cannot be streaming through us; treat the
			// successful Stop above as sufficient.
			return nil
		}
		if state != "play" && state != "pause" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

func (e *Engine) setHolderLocked(h Holder) {
	e.active = h.Source
	e.holderPeer = h.peer()
	e.holderToken = h.token()
	e.evict = h.Evict
	e.stopStreak = 0
	e.logger.Info("sound card claimed",
		"source", h.Source, "peer", h.peer(), "token", h.token(), "preemptive", h.Evict != nil)
}

// PlaybackInfo is the unified playback snapshot.
type PlaybackInfo struct {
	// State is the kernel transport state: play, pause, stop or unknown.
	State string `json:"state"`
	// Elapsed / Duration are seconds of the current track.
	Elapsed  float64 `json:"elapsed"`
	Duration float64 `json:"duration"`
	// Volume is the hardware mixer percentage; Muted its switch state.
	Volume int  `json:"volume"`
	Muted  bool `json:"muted"`
	// URI / Title / Artist / Album describe the current queue entry.
	URI    string `json:"uri,omitempty"`
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
	// Error carries a kernel access problem without failing the read.
	Error string `json:"error,omitempty"`
}

// Player is the playback surface the DLNA renderer drives. *Engine implements
// it; declaring it here also keeps the renderer unit-testable with a fake.
type Player interface {
	ClaimDLNA() error
	ReleaseDLNA()
	ActiveSource() Source
	Load(ctx context.Context, uri string) error
	Clear(ctx context.Context) error
	Play(ctx context.Context) error
	Pause(ctx context.Context) error
	Stop(ctx context.Context) error
	Seek(ctx context.Context, seconds float64) error
	Info(ctx context.Context) PlaybackInfo
	SetVolume(ctx context.Context, percent int) (int, error)
	SetMute(ctx context.Context, muted bool) (bool, error)
	Volume() (int, bool)
}

// Engine arbitrates the sound card between the MiPlay pipeline and the MPD
// kernel and proxies the kernel surface.
type Engine struct {
	cfg     config.Config
	logger  *slog.Logger
	manager *mpd.Manager
	client  *mpd.Client
	mixer   *alsa.Mixer

	mu     sync.Mutex
	active Source
	// holderPeer identifies the sender among senders of the same Source (two
	// phones casting MiPlay are two peers of one Source).
	holderPeer string
	// holderToken identifies the session instance, so a superseded session
	// unwinding late cannot free its successor's slot.
	holderToken string
	// evict is the current holder's teardown; nil means "stop the kernel".
	evict func(ctx context.Context) error
	// handoff serialises hand-offs. It is held across the eviction wait, which
	// happens with mu released.
	handoff sync.Mutex
	// stopStreak counts consecutive monitor ticks that observed a stopped
	// kernel while dlna/api held the slot. Two in a row release it: one lone
	// "stop" is a normal step inside Clear -> Add -> Play.
	stopStreak int
}

// New assembles the engine around an already running playback kernel.
func New(cfg config.Config, logger *slog.Logger, manager *mpd.Manager, mixer *alsa.Mixer) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	engine := &Engine{
		cfg:     cfg,
		logger:  logger,
		manager: manager,
		mixer:   mixer,
		active:  SourceIdle,
	}
	if manager != nil {
		engine.client = manager.Client()
	}
	return engine
}

// Client exposes the raw MPD client for the read-only status paths that keep
// the original local-output response shapes.
func (e *Engine) Client() *mpd.Client { return e.client }

// Manager exposes the supervised playback kernel for /healthz reporting.
func (e *Engine) Manager() *mpd.Manager { return e.manager }

// ---------------------------------------------------------------------------
// Arbitration
// ---------------------------------------------------------------------------

// Claim reserves the card for one source, refusing when another source already
// holds it. MiPlay additionally verifies that the kernel is not mid-stream,
// covering a lost release.
//
// This is the non-preemptive path (the HTTP API and the test tone): a busy card
// answers "busy" instead of interrupting whoever is playing. The cast sources
// (MiPlay, DLNA) go through Acquire, which takes over.
func (e *Engine) Claim(source Source) error {
	if source == SourceIdle {
		return errors.New("the idle source cannot claim the sound card")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == source {
		return nil
	}
	if e.active != SourceIdle {
		return fmt.Errorf("%w: %s is active", ErrSourceBusy, e.active)
	}
	if source == SourceMiPlay {
		if state, err := e.kernelState(); err == nil && (state == "play" || state == "pause") {
			return fmt.Errorf("%w: the playback kernel is still streaming", ErrSourceBusy)
		}
	}
	e.setHolderLocked(Holder{Source: source})
	return nil
}

// Release frees the slot when the given source still owns it. Idempotent.
func (e *Engine) Release(source Source) {
	e.ReleaseHolder(Holder{Source: source})
}

// ReleaseHolder frees the slot only when that exact session still owns it.
// Token-matched on purpose: a cast that was preempted (or whose phone
// reconnected) unwinds after the fact, and must not evict the cast that
// replaced it.
func (e *Engine) ReleaseHolder(h Holder) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active != h.Source || e.holderToken != h.token() {
		return
	}
	e.releaseLocked(h.Source)
}

// ReleaseSource forces the slot back to idle regardless of the owner and
// reports who held it. It is the safety net behind stop_all and the console.
func (e *Engine) ReleaseSource() Source {
	e.mu.Lock()
	defer e.mu.Unlock()
	previous := e.active
	e.releaseLocked(previous)
	return previous
}

func (e *Engine) releaseLocked(source Source) {
	if source == SourceIdle || e.active != source {
		return
	}
	e.active = SourceIdle
	e.holderPeer = ""
	e.holderToken = ""
	e.evict = nil
	e.stopStreak = 0
	e.logger.Info("sound card released", "source", source)
}

// CurrentHolder returns the token of the session that currently owns the card
// ("" when idle). Two casts of the same Source are told apart by this token,
// which is what makes "the same phone reconnected" distinguishable from "a
// different phone took over" -- for /api/status as well as for the tests.
func (e *Engine) CurrentHolder() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.holderToken
}

// ActiveSource reports the current owner for /api/status.
func (e *Engine) ActiveSource() Source {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active
}

// ClaimDLNA / ReleaseDLNA are the Player interface spellings of the DLNA
// source's arbitration.
//
// ClaimDLNA is preemptive: a UPnP controller pressing Play is a new cast, and a
// new cast wins. Whatever held the card (a MiPlay cast in particular) is stopped
// and waited for first, so the two can never stream into one exclusive device.
func (e *Engine) ClaimDLNA() error {
	return e.Acquire(context.Background(), Holder{Source: SourceDLNA, Peer: "dlna", Token: "dlna"})
}

// ReleaseDLNA frees the DLNA slot if DLNA still owns it.
func (e *Engine) ReleaseDLNA() { e.Release(SourceDLNA) }

// ---------------------------------------------------------------------------
// Kernel surface (Player implementation + API primitives)
// ---------------------------------------------------------------------------

// Load prepares the queue with exactly one URI, stopped.
func (e *Engine) Load(ctx context.Context, uri string) error {
	if err := e.client.Clear(ctx); err != nil {
		return err
	}
	return e.client.Add(ctx, uri)
}

// Clear empties the queue; playback of a cleared queue stops.
func (e *Engine) Clear(ctx context.Context) error {
	if e.client == nil {
		return mpd.ErrUnavailable
	}
	return e.client.Clear(ctx)
}

// Play starts (or resumes) kernel playback.
func (e *Engine) Play(ctx context.Context) error {
	if e.client == nil {
		return mpd.ErrUnavailable
	}
	return e.client.Play(ctx)
}

// Pause pauses kernel playback.
func (e *Engine) Pause(ctx context.Context) error {
	if e.client == nil {
		return mpd.ErrUnavailable
	}
	return e.client.Pause(ctx, true)
}

// Stop halts the kernel and ends whatever dlna/api claim was active: a
// stopped kernel releases the ALSA device by itself.
func (e *Engine) Stop(ctx context.Context) error {
	if e.client == nil {
		return mpd.ErrUnavailable
	}
	if err := e.client.Stop(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.releaseLocked(SourceDLNA)
	e.releaseLocked(SourceAPI)
	e.mu.Unlock()
	return nil
}

// Seek moves within the current queue entry.
func (e *Engine) Seek(ctx context.Context, seconds float64) error {
	if e.client == nil {
		return mpd.ErrUnavailable
	}
	if seconds < 0 {
		seconds = 0
	}
	return e.client.SeekCur(ctx, time.Duration(seconds*float64(time.Second)))
}

// Info reads one unified playback snapshot. Failures are reported inside the
// struct so polling callers never need error branches for cosmetic reads.
func (e *Engine) Info(ctx context.Context) PlaybackInfo {
	info := PlaybackInfo{State: "unknown", Volume: -1}
	if e.mixer != nil {
		state := e.mixer.State()
		info.Volume = state.VolumePercent
		info.Muted = state.Muted
	}
	if e.client == nil {
		info.Error = "playback kernel is unavailable"
		return info
	}
	status, err := e.client.Status(ctx)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.State = status["state"]
	info.Elapsed = atofOr(status["elapsed"], 0)
	info.Duration = atofOr(status["duration"], 0)
	if song, songErr := e.client.CurrentSong(ctx); songErr == nil && song != nil {
		info.URI = song["file"]
		info.Title = song["Title"]
		info.Artist = song["Artist"]
		info.Album = song["Album"]
		if info.Duration == 0 {
			info.Duration = atofOr(song["duration"], 0)
		}
	}
	return info
}

// SetVolume writes the hardware mixer (the volume authority shared by
// MiPlay, DLNA and the API) and mirrors the level into the kernel so MPD's
// cached volume does not drift.
func (e *Engine) SetVolume(ctx context.Context, percent int) (int, error) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	if e.mixer != nil && e.mixer.State().Initialized {
		state, err := e.mixer.SetVolume(ctx, percent)
		if err != nil {
			return state.VolumePercent, err
		}
		if e.client != nil {
			if err := e.client.SetVolume(ctx, percent); err != nil {
				e.logger.Debug("could not mirror the volume into the playback kernel", "error", err)
			}
		}
		return state.VolumePercent, nil
	}
	if e.client == nil {
		return 0, mpd.ErrUnavailable
	}
	if err := e.client.SetVolume(ctx, percent); err != nil {
		return 0, err
	}
	return percent, nil
}

// SetMute flips the hardware switch of the effective control.
func (e *Engine) SetMute(ctx context.Context, muted bool) (bool, error) {
	if e.mixer == nil || !e.mixer.State().Initialized {
		return false, errors.New("no hardware mixer is available to mute")
	}
	state, err := e.mixer.SetMute(ctx, muted)
	if err != nil {
		return state.Muted, err
	}
	return state.Muted, nil
}

// Volume reports the mixer level and switch state.
func (e *Engine) Volume() (int, bool) {
	if e.mixer == nil {
		return 0, false
	}
	state := e.mixer.State()
	return state.VolumePercent, state.Muted
}

// RefreshMixer re-reads the hardware so status endpoints report the truth.
func (e *Engine) RefreshMixer(ctx context.Context) {
	if e.mixer == nil {
		return
	}
	if _, err := e.mixer.Refresh(ctx); err != nil {
		e.logger.Debug("could not refresh the mixer snapshot", "error", err)
	}
}

// Supervise watches for a kernel that stopped on its own (natural end of a
// stream) while dlna/api still held the slot, and releases it.
func (e *Engine) Supervise(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			e.mu.Lock()
			source := e.active
			e.mu.Unlock()
			if source != SourceDLNA && source != SourceAPI {
				continue
			}
			state, err := e.kernelState()
			if err != nil {
				continue
			}
			e.mu.Lock()
			if e.active == source && state == "stop" {
				e.stopStreak++
				if e.stopStreak >= 2 {
					e.logger.Info("playback kernel stopped on its own; releasing the slot", "source", source)
					e.releaseLocked(source)
				}
			} else {
				e.stopStreak = 0
			}
			e.mu.Unlock()
		}
	}()
}

// kernelState reads the current transport state with a short timeout; it is
// called while the arbitration mutex is held, so it must never block long.
func (e *Engine) kernelState() (string, error) {
	if e.client == nil {
		return "", mpd.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := e.client.Status(ctx)
	if err != nil {
		return "", err
	}
	return status["state"], nil
}

func atofOr(raw string, fallback float64) float64 {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return fallback
	}
	return value
}
