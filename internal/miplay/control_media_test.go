package miplay

import (
	"testing"
	"time"
)

// newTestSession builds a bare control session for metadata tests (no socket,
// no Safety channel).
func newTestSession(t *testing.T) *LegacyReceiverSession {
	t.Helper()
	session, err := NewLegacyReceiverSession([]byte("123456789012"), 7, "bridge", 50,
		"127.0.0.1", 8899, "10.0.0.2", 5000)
	if err != nil {
		t.Fatalf("NewLegacyReceiverSession: %v", err)
	}
	return session
}

// The console draws its progress bar from these fields, so they have to survive
// the exact payload shape the phone sends (m-prefixed keys, milliseconds).
func TestCaptureMediaInfoReadsDurationAndCover(t *testing.T) {
	session := newTestSession(t)

	session.captureMediaInfo(map[string]any{
		"mTitle":    "第一首",
		"mArtist":   "歌手",
		"mAlbum":    "专辑",
		"mDuration": float64(270800), // 真机实测：4 分 31 秒，单位毫秒
		"mCoverUrl": "http://10.0.0.2:8080/cover.jpg",
	})

	if got := session.MediaInfo()["title"]; got != "第一首" {
		t.Fatalf("title = %q", got)
	}
	if got := session.MediaInfo()["artist"]; got != "歌手" {
		t.Fatalf("artist = %q", got)
	}
	if got := session.DurationSeconds(); got != 270.8 {
		t.Fatalf("duration = %v, want 270.8", got)
	}
	if got := session.CoverURL(); got != "http://10.0.0.2:8080/cover.jpg" {
		t.Fatalf("cover = %q", got)
	}
	if got := session.TrackVersion(); got != 1 {
		t.Fatalf("track version = %d, want 1", got)
	}
}

// A repeated metadata frame for the same song must not look like a track
// change: the console would otherwise reset its playhead every few seconds.
func TestCaptureMediaInfoKeepsVersionForSameTrack(t *testing.T) {
	session := newTestSession(t)
	session.captureMediaInfo(map[string]any{"mTitle": "同一首", "mDuration": float64(200000)})
	session.captureMediaInfo(map[string]any{"mTitle": "同一首", "mDuration": float64(200000)})

	if got := session.TrackVersion(); got != 1 {
		t.Fatalf("track version = %d, want 1", got)
	}
	if got := session.DurationSeconds(); got != 200 {
		t.Fatalf("duration = %v, want 200", got)
	}
}

// A new title means a new song, and the previous song's length/artwork must not
// leak into it: 否则没带元数据的那一首会沿用上一首的时长，进度条直接失真。
func TestCaptureMediaInfoResetsPerTrackFields(t *testing.T) {
	session := newTestSession(t)
	session.captureMediaInfo(map[string]any{
		"mTitle":    "第一首",
		"mDuration": float64(270800),
		"mCoverUrl": "http://10.0.0.2/c1.jpg",
	})

	session.captureMediaInfo(map[string]any{"mTitle": "第二首"})
	if got := session.TrackVersion(); got != 2 {
		t.Fatalf("track version = %d, want 2", got)
	}
	if got := session.DurationSeconds(); got != 0 {
		t.Fatalf("duration leaked from the previous track: %v", got)
	}
	if got := session.CoverURL(); got != "" {
		t.Fatalf("cover leaked from the previous track: %q", got)
	}
	if got := session.MediaInfo()["title"]; got != "第二首" {
		t.Fatalf("title = %q, want 第二首", got)
	}

	// 新曲目自带的元数据照常读到（字符串形态也认）。
	session.captureMediaInfo(map[string]any{
		"mTitle":    "第三首",
		"mDuration": "185000",
		"mCoverUrl": "http://10.0.0.2/c3.jpg",
	})
	if got := session.DurationSeconds(); got != 185 {
		t.Fatalf("duration = %v, want 185", got)
	}
	if got := session.CoverURL(); got != "http://10.0.0.2/c3.jpg" {
		t.Fatalf("cover = %q", got)
	}
}

// Metadata hidden one level down (mediaInfo / media_info) is still read.
func TestCaptureMediaInfoReadsNestedPayload(t *testing.T) {
	session := newTestSession(t)
	session.captureMediaInfo(map[string]any{
		"mediaInfo": map[string]any{
			"mTitle":    "嵌套",
			"mDuration": float64(60000),
		},
	})
	if got := session.MediaInfo()["title"]; got != "嵌套" {
		t.Fatalf("title = %q", got)
	}
	if got := session.DurationSeconds(); got != 60 {
		t.Fatalf("duration = %v, want 60", got)
	}
}

func TestMediaDurationSecondsRejectsImplausibleValues(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want float64
		ok   bool
	}{
		{"毫秒", float64(270800), 270.8, true},
		{"字符串毫秒", "185000", 185, true},
		{"秒不会被当成毫秒", float64(240), 0, false},
		{"零", float64(0), 0, false},
		{"负数", float64(-1), 0, false},
		{"超过一天", float64(25 * 60 * 60 * 1000), 0, false},
		{"非数字字符串", "abc", 0, false},
		{"缺失", nil, 0, false},
		{"布尔", true, 0, false},
	}
	for _, tc := range cases {
		got, ok := mediaDurationSeconds(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: mediaDurationSeconds(%v) = (%v, %v), want (%v, %v)",
				tc.name, tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// The playhead ledger is what the console's progress bar reads; it has to start
// on the first frame, freeze while paused, and restart on a track change.
func TestSessionRuntimePlayhead(t *testing.T) {
	runtime := &sessionRuntime{trackVersion: -1}

	if _, measured := runtime.elapsed(); measured {
		t.Fatal("elapsed reported before any media frame")
	}

	runtime.markMediaStart()
	runtime.noteTrack(0)
	first, measured := runtime.elapsed()
	if !measured {
		t.Fatal("elapsed not measured after the first media frame")
	}
	if first < 0 {
		t.Fatalf("elapsed = %v", first)
	}

	runtime.setPaused(true)
	if !runtime.isPaused() {
		t.Fatal("paused flag not set")
	}
	runtime.clearPause()
	if runtime.isPaused() {
		t.Fatal("media arrival must clear a stale pause hint")
	}

	// 换歌：时钟归零，进度条重新开始。
	runtime.mediaStartedAt = time.Now().Add(-time.Hour)
	runtime.noteTrack(1)
	rebased, _ := runtime.elapsed()
	if rebased > time.Minute {
		t.Fatalf("playhead was not rebased for the new track: %v", rebased)
	}
}
