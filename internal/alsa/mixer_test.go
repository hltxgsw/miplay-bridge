package alsa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
)

func TestParseSimpleControls(t *testing.T) {
	output := `Simple mixer control 'Master',0
Simple mixer control 'Headphone',0
Simple mixer control 'Auto-Mute Mode',0
`
	controls := ParseSimpleControls(output)
	want := []string{"Master", "Headphone", "Auto-Mute Mode"}
	if len(controls) != len(want) {
		t.Fatalf("controls = %v, want %v", controls, want)
	}
	for index := range want {
		if controls[index] != want[index] {
			t.Fatalf("controls[%d] = %q, want %q", index, controls[index], want[index])
		}
	}
}

func TestParseItemValue(t *testing.T) {
	output := `Simple mixer control 'Auto-Mute Mode',0
  Capabilities: enum
  Items: 'Disabled' 'Enabled'
  Item0: 'Disabled'
`
	value, ok := ParseItemValue(output, 0)
	if !ok || value != "Disabled" {
		t.Fatalf("ParseItemValue = %q, %v", value, ok)
	}
	if _, ok := ParseItemValue(output, 3); ok {
		t.Fatal("expected Item3 to be absent")
	}
}

func TestParsePercentAndSwitchState(t *testing.T) {
	output := `Simple mixer control 'Headphone',0
  Limits: Playback 0 - 87
  Front Left: Playback 65 [75%] [-16.50dB] [on]
  Front Right: Playback 65 [75%] [-16.50dB] [on]
`
	percent, ok := ParsePercent(output)
	if !ok || percent != 75 {
		t.Fatalf("ParsePercent = %d, %v", percent, ok)
	}
	muted, ok := ParseSwitchState(output)
	if !ok || muted {
		t.Fatalf("ParseSwitchState = %v, %v; want unmuted", muted, ok)
	}

	mutedOutput := `  Mono: Playback 0 [0%] [-65.25dB] [off]`
	muted, ok = ParseSwitchState(mutedOutput)
	if !ok || !muted {
		t.Fatalf("ParseSwitchState = %v, %v; want muted", muted, ok)
	}
}

// fakeAmixer emulates the subset of alsa-utils the bridge uses.
type fakeAmixer struct {
	mu       sync.Mutex
	autoMute string
	controls []string
	switches map[string]string
	percent  map[string]int
	failSet  map[string]error
	calls    []string
}

func newFakeAmixer() *fakeAmixer {
	return &fakeAmixer{
		autoMute: "Enabled",
		controls: []string{"Master", "Headphone", "Speaker", "Auto-Mute Mode"},
		switches: map[string]string{"Master": "off", "Headphone": "off", "Speaker": "off"},
		percent:  map[string]int{"Master": 70, "Headphone": 65, "Speaker": 65},
		failSet:  map[string]error{},
	}
}

func (f *fakeAmixer) run(_ context.Context, name string, args ...string) (string, string, error) {
	if name != "amixer" {
		return "", "", fmt.Errorf("unexpected binary %q", name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Drop the "-c <card>" prefix.
	if len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	if len(args) == 0 {
		return "", "", errors.New("no arguments")
	}
	f.calls = append(f.calls, strings.Join(args, " "))

	switch args[0] {
	case "scontrols":
		var builder strings.Builder
		for _, control := range f.controls {
			fmt.Fprintf(&builder, "Simple mixer control '%s',0\n", control)
		}
		return builder.String(), "", nil
	case "sget":
		control := args[1]
		switch control {
		case "Auto-Mute Mode":
			return fmt.Sprintf("Simple mixer control 'Auto-Mute Mode',0\n  Items: 'Disabled' 'Enabled'\n  Item0: '%s'\n", f.autoMute), "", nil
		default:
			return fmt.Sprintf("Simple mixer control '%s',0\n  Front Left: Playback %d [%d%%] [0.00dB] [%s]\n", control, f.percent[control], f.percent[control], f.switches[control]), "", nil
		}
	case "sset":
		control := args[1]
		if err, ok := f.failSet[control]; ok {
			return "", "amixer: error", err
		}
		switch control {
		case "Auto-Mute Mode":
			if len(args) > 2 {
				f.autoMute = args[2]
			}
		default:
			for _, value := range args[2:] {
				if value == "unmute" {
					f.switches[control] = "on"
				}
				if value == "mute" {
					f.switches[control] = "off"
				}
				if strings.HasSuffix(value, "%") {
					var percent int
					fmt.Sscanf(strings.TrimSuffix(value, "%"), "%d", &percent)
					f.percent[control] = percent
				}
			}
		}
		return "", "", nil
	default:
		return "", "", fmt.Errorf("unsupported amixer command %q", args[0])
	}
}

func testConfig() config.Config {
	cfg := config.Default()
	return cfg
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestInitDisablesAutoMuteAndUnmutes(t *testing.T) {
	fake := newFakeAmixer()
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)

	state, err := mixer.Init(context.Background())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !state.AutoMuteFixed {
		t.Fatal("AutoMuteFixed must be true after a successful Init")
	}
	if state.AutoMuteState != "Disabled" {
		t.Fatalf("AutoMuteState = %q", state.AutoMuteState)
	}
	if !state.Initialized || !state.Verified {
		t.Fatalf("state = %+v", state)
	}
	if state.EffectiveControl != "Headphone" {
		t.Fatalf("EffectiveControl = %q, want Headphone", state.EffectiveControl)
	}
	if len(state.Unmuted) != 3 {
		t.Fatalf("Unmuted = %v, want Master/Headphone/Speaker", state.Unmuted)
	}
	if fake.autoMute != "Disabled" {
		t.Fatalf("fake autoMute = %q", fake.autoMute)
	}
	if fake.switches["Headphone"] != "on" {
		t.Fatal("Headphone must be unmuted")
	}

	// The Auto-Mute fix must be the first write, before any unmute.
	if len(fake.calls) < 2 || !strings.HasPrefix(fake.calls[1], "sset Auto-Mute Mode Disabled") {
		t.Fatalf("first mixer write = %v, want the Auto-Mute fix", fake.calls)
	}
}

func TestInitFailsWhenAutoMuteCannotBeDisabled(t *testing.T) {
	fake := newFakeAmixer()
	fake.failSet["Auto-Mute Mode"] = errors.New("amixer: Unable to find simple control")
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)

	_, err := mixer.Init(context.Background())
	if err == nil {
		t.Fatal("Init must fail when the Auto-Mute fix cannot be applied: otherwise the container is green but silent")
	}
}

func TestInitFailsWhenAutoMuteReadBackStaysEnabled(t *testing.T) {
	fake := newFakeAmixer()
	// Simulate a card that accepts the write but keeps the old value.
	mixer := New(testConfig(), testLogger()).WithRunner(func(ctx context.Context, name string, args ...string) (string, string, error) {
		stdout, stderr, err := fake.run(ctx, name, args...)
		if len(args) >= 4 && args[0] == "-c" && args[2] == "sget" && args[3] == "Auto-Mute Mode" {
			return "Simple mixer control 'Auto-Mute Mode',0\n  Item0: 'Enabled'\n", stderr, err
		}
		return stdout, stderr, err
	})

	_, err := mixer.Init(context.Background())
	if err == nil {
		t.Fatal("Init must fail when Auto-Mute Mode still reads Enabled")
	}
}

func TestInitToleratesCardsWithoutAutoMute(t *testing.T) {
	fake := newFakeAmixer()
	fake.controls = []string{"Master", "Speaker"}
	fake.switches["Headphone"] = "on"
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)

	state, err := mixer.Init(context.Background())
	if err != nil {
		t.Fatalf("Init on a card without Auto-Mute Mode: %v", err)
	}
	if state.AutoMuteFixed {
		t.Fatal("AutoMuteFixed must stay false when the card has no such control")
	}
	if state.AutoMuteState != "unsupported" {
		t.Fatalf("AutoMuteState = %q, want unsupported", state.AutoMuteState)
	}
	if state.EffectiveControl != "Master" {
		t.Fatalf("EffectiveControl = %q, want the Master fallback", state.EffectiveControl)
	}
}

func TestSetVolumeAndRefresh(t *testing.T) {
	fake := newFakeAmixer()
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)
	if _, err := mixer.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	state, err := mixer.SetVolume(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if state.VolumePercent != 42 {
		t.Fatalf("VolumePercent = %d", state.VolumePercent)
	}

	fake.mu.Lock()
	fake.percent["Headphone"] = 55
	fake.mu.Unlock()
	refreshed, err := mixer.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.VolumePercent != 55 {
		t.Fatalf("refreshed VolumePercent = %d, want 55", refreshed.VolumePercent)
	}
}

func TestSetMuteAndRefresh(t *testing.T) {
	fake := newFakeAmixer()
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)
	if _, err := mixer.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	state, err := mixer.SetMute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Muted {
		t.Fatalf("state = %+v, want muted", state)
	}
	if fake.switches["Headphone"] != "off" {
		t.Fatalf("Headphone switch = %q, want off", fake.switches["Headphone"])
	}

	state, err = mixer.SetMute(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if state.Muted {
		t.Fatalf("state = %+v, want unmuted", state)
	}
}

func TestInitSupportsPCMControlOnly(t *testing.T) {
	fake := newFakeAmixer()
	fake.controls = []string{"PCM"}
	fake.switches = map[string]string{"PCM": "on"}
	fake.percent = map[string]int{"PCM": 80}
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)

	state, err := mixer.Init(context.Background())
	if err != nil {
		t.Fatalf("Init on an ARM card with PCM only: %v", err)
	}
	if state.EffectiveControl != "PCM" {
		t.Fatalf("EffectiveControl = %q, want PCM", state.EffectiveControl)
	}
	if !state.Verified {
		t.Fatal("state.Verified must be true for PCM")
	}
}

func TestInitToleratesCardsWithoutAnyHardwareMixer(t *testing.T) {
	fake := newFakeAmixer()
	fake.controls = []string{}
	fake.switches = map[string]string{}
	fake.percent = map[string]int{}
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)

	state, err := mixer.Init(context.Background())
	if err != nil {
		t.Fatalf("Init on a card without hardware mixer: %v", err)
	}
	if state.EffectiveControl != "" {
		t.Fatalf("EffectiveControl = %q, want empty (software volume fallback)", state.EffectiveControl)
	}
	if !state.Verified {
		t.Fatal("state.Verified must be true when falling back to software volume")
	}
}

func TestVolumeCurveDBMapping(t *testing.T) {
	curve := []config.VolumeCurvePoint{
		{Percent: 0, DB: -60},
		{Percent: 30, DB: -30},
		{Percent: 50, DB: -20},
		{Percent: 80, DB: -10},
		{Percent: 100, DB: 0},
	}
	cases := []struct {
		percent int
		want    string
	}{
		{0, "-60dB"},   // 下端点
		{30, "-30dB"},  // 中间断点
		{50, "-20dB"},  // 中间断点
		{80, "-10dB"},  // 中间断点
		{100, "0dB"},   // 上端点
		{15, "-45dB"},  // 断点之间线性插值
		{-10, "-60dB"}, // 下越界 clamp 到 0%
		{150, "0dB"},   // 上越界 clamp 到 100%
	}
	for _, testCase := range cases {
		got, ok := volumeCurveDB(curve, testCase.percent)
		if !ok || got != testCase.want {
			t.Errorf("percent %d => (%q, %v), want %q", testCase.percent, got, ok, testCase.want)
		}
	}
}

func TestVolumeCurveDBDisabled(t *testing.T) {
	if got, ok := volumeCurveDB(nil, 50); ok || got != "" {
		t.Fatalf("nil curve => (%q, %v), want empty and false", got, ok)
	}
	single := []config.VolumeCurvePoint{{Percent: 0, DB: -60}}
	if got, ok := volumeCurveDB(single, 50); ok || got != "" {
		t.Fatalf("single-point curve => (%q, %v), want empty and false", got, ok)
	}
}

func TestSetVolumeWritesDBWhenCurveConfigured(t *testing.T) {
	cfg := testConfig()
	cfg.VolumeCurve = []config.VolumeCurvePoint{
		{Percent: 0, DB: -60}, {Percent: 30, DB: -30}, {Percent: 50, DB: -20},
		{Percent: 80, DB: -10}, {Percent: 100, DB: 0},
	}
	fake := newFakeAmixer()
	mixer := New(cfg, testLogger()).WithRunner(fake.run)
	if _, err := mixer.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := mixer.SetVolume(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	joined := strings.Join(fake.calls, "\n")
	fake.mu.Unlock()
	// 回归：负 dB 必须以 "--" 与选项分隔，否则 amixer 报 "invalid option -- '2'"
	// 并静默失败（实测 amixer 1.2.14）。
	if !strings.Contains(joined, "sset Headphone -- -20dB unmute") {
		t.Fatalf("配置曲线后应以 '-- -20dB' 下发，实际调用:\n%s", joined)
	}
	if strings.Contains(joined, "sset Headphone 50%") {
		t.Fatalf("配置曲线后不应再以 %% 下发:\n%s", joined)
	}
}

func TestSetVolumeKeepsPercentWithoutCurve(t *testing.T) {
	fake := newFakeAmixer()
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)
	if _, err := mixer.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := mixer.SetVolume(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	joined := strings.Join(fake.calls, "\n")
	fake.mu.Unlock()
	if !strings.Contains(joined, "sset Headphone 50%") {
		t.Fatalf("未配置曲线时必须保持上游 %% 行为，实际调用:\n%s", joined)
	}
	if strings.Contains(joined, "dB") || strings.Contains(joined, " -- ") {
		t.Fatalf("未配置曲线时不应出现 dB 或 --:\n%s", joined)
	}
}
