package config

import "testing"

func TestDefaultMatchesVerifiedHardware(t *testing.T) {
	cfg := Default()
	if cfg.Port != 8092 {
		t.Fatalf("default port = %d, want 8092 (8091 belongs to the local-output sidecar)", cfg.Port)
	}
	if cfg.Card != 0 || cfg.PCMDevice != 0 {
		t.Fatalf("default card/pcm = %d/%d, want 0/0", cfg.Card, cfg.PCMDevice)
	}
	if got := cfg.ALSAHardwareDevice(); got != "hw:0,0" {
		t.Fatalf("ALSAHardwareDevice() = %q, want hw:0,0", got)
	}
	if got := cfg.ALSAHardwareCard(); got != "hw:0" {
		t.Fatalf("ALSAHardwareCard() = %q, want hw:0", got)
	}
	if cfg.AutoMuteControl != "Auto-Mute Mode" {
		t.Fatalf("AutoMuteControl = %q, want Auto-Mute Mode", cfg.AutoMuteControl)
	}
	if !cfg.FixAutoMute {
		t.Fatal("FixAutoMute must default to true: without it the 3.5mm jack stays muted")
	}
	if cfg.MPDMixerControl != "Headphone" {
		t.Fatalf("MPDMixerControl = %q, want Headphone", cfg.MPDMixerControl)
	}
	if cfg.SoundDirectory != "/dev/snd" {
		t.Fatalf("SoundDirectory = %q, want /dev/snd", cfg.SoundDirectory)
	}
	if got := cfg.ControlDevicePath(); got != "/dev/snd/controlC0" {
		t.Fatalf("ControlDevicePath() = %q", got)
	}
	if got := cfg.PCMPlaybackDevicePath(); got != "/dev/snd/pcmC0D0p" {
		t.Fatalf("PCMPlaybackDevicePath() = %q", got)
	}
}

func TestDefaultCoversReceiverAndRenderer(t *testing.T) {
	cfg := Default()
	if !cfg.MiPlayEnabled {
		t.Fatal("MiPlayEnabled must default to true: the receiver is the whole point")
	}
	if cfg.MiPlayControlPort != 8899 {
		t.Fatalf("MiPlayControlPort = %d, want the Mi-connect convention 8899", cfg.MiPlayControlPort)
	}
	if cfg.MiPlayAlsaBufferUS != 200000 {
		t.Fatalf("MiPlayAlsaBufferUS = %d, want 200000", cfg.MiPlayAlsaBufferUS)
	}
	if !cfg.DLNAEnabled {
		t.Fatal("DLNAEnabled must default to true")
	}
	if cfg.DataDirectory != "/data" {
		t.Fatalf("DataDirectory = %q, want /data so identities survive restarts", cfg.DataDirectory)
	}
	if got := cfg.MiPlayDeviceIDFile(); got != "/data/miplay-device-id" {
		t.Fatalf("MiPlayDeviceIDFile() = %q", got)
	}
	if got := cfg.DLNAUDNFile(); got != "/data/dlna-udn" {
		t.Fatalf("DLNAUDNFile() = %q", got)
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("BRIDGE_PORT", "9001")
	t.Setenv("BRIDGE_TOKEN", "s3cret")
	t.Setenv("ALSA_CARD", "2")
	t.Setenv("ALSA_PCM_DEVICE", "3")
	t.Setenv("MPD_MIXER_CONTROL", "Master")
	t.Setenv("ALSA_FIX_AUTO_MUTE", "false")
	t.Setenv("BRIDGE_TEST_TONE_HZ", "1000")
	t.Setenv("MIPLAY_CONTROL_PORT", "9987")
	t.Setenv("MIPLAY_ALSA_BUFFER_US", "350000")
	t.Setenv("DLNA_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9001 {
		t.Fatalf("Port = %d", cfg.Port)
	}
	if cfg.Token != "s3cret" {
		t.Fatalf("Token = %q", cfg.Token)
	}
	if cfg.Card != 2 || cfg.PCMDevice != 3 {
		t.Fatalf("card/pcm = %d/%d", cfg.Card, cfg.PCMDevice)
	}
	if got := cfg.ALSAHardwareDevice(); got != "hw:2,3" {
		t.Fatalf("ALSAHardwareDevice() = %q", got)
	}
	if cfg.MPDMixerControl != "Master" {
		t.Fatalf("MPDMixerControl = %q", cfg.MPDMixerControl)
	}
	if cfg.FixAutoMute {
		t.Fatal("ALSA_FIX_AUTO_MUTE=false must disable the fix")
	}
	if cfg.TestToneFrequencyHz != 1000 {
		t.Fatalf("TestToneFrequencyHz = %d", cfg.TestToneFrequencyHz)
	}
	if cfg.MiPlayControlPort != 9987 {
		t.Fatalf("MiPlayControlPort = %d", cfg.MiPlayControlPort)
	}
	if cfg.MiPlayAlsaBufferUS != 350000 {
		t.Fatalf("MiPlayAlsaBufferUS = %d", cfg.MiPlayAlsaBufferUS)
	}
	if cfg.DLNAEnabled {
		t.Fatal("DLNA_ENABLED=false must disable the renderer")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := []struct{ name, value string }{
		{"BRIDGE_PORT", "70000"},
		{"MIPLAY_CONTROL_PORT", "0"},
		{"MIPLAY_ALSA_BUFFER_US", "1"},
		{"DLNA_SSDP_MAX_AGE", "5"},
		{"ALSA_DEFAULT_VOLUME", "101"},
		{"ALSA_FIX_AUTO_MUTE", "maybe"},
		{"DLNA_ENABLED", "maybe"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(testCase.name, testCase.value)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%s must be rejected", testCase.name, testCase.value)
			}
		})
	}
}

func TestParseVolumeCurve(t *testing.T) {
	points, err := ParseVolumeCurve("0:-60,30:-30,50:-20,80:-10,100:0")
	if err != nil {
		t.Fatalf("ParseVolumeCurve: %v", err)
	}
	want := []VolumeCurvePoint{{0, -60}, {30, -30}, {50, -20}, {80, -10}, {100, 0}}
	if len(points) != len(want) {
		t.Fatalf("points = %v, want %v", points, want)
	}
	for index := range want {
		if points[index] != want[index] {
			t.Fatalf("points[%d] = %v, want %v", index, points[index], want[index])
		}
	}
}

func TestParseVolumeCurveRejects(t *testing.T) {
	for _, spec := range []string{
		"",                          // 空串
		"0:-60",                     // 只有一个点
		"10:-60,100:0",              // 未从 0 起
		"0:-60,90:0",                // 未到 100
		"0:-60,30:-30,30:-20,100:0", // 百分比不递增
		"0:-60,x:0",                 // dB 非数字
		"abc",                       // 缺少冒号
		"0:5,100:0",                 // dB 为正（这是衰减器）
		"0:-60,101:0",               // 百分比越界
	} {
		if _, err := ParseVolumeCurve(spec); err == nil {
			t.Errorf("ParseVolumeCurve(%q) must fail", spec)
		}
	}
}

func TestDefaultVolumeCurveIsUpstream(t *testing.T) {
	if len(Default().VolumeCurve) != 0 {
		t.Fatal("VolumeCurve must default to empty: upstream percent behaviour")
	}
}

func TestLoadReadsVolumeCurve(t *testing.T) {
	t.Setenv("ALSA_VOLUME_CURVE", "0:-60,100:0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.VolumeCurve) != 2 || cfg.VolumeCurve[0].DB != -60 || cfg.VolumeCurve[1].Percent != 100 {
		t.Fatalf("VolumeCurve = %v", cfg.VolumeCurve)
	}
}

func TestLoadRejectsBadVolumeCurve(t *testing.T) {
	t.Setenv("ALSA_VOLUME_CURVE", "oops")
	if _, err := Load(); err == nil {
		t.Fatal("a malformed ALSA_VOLUME_CURVE must fail Load")
	}
}

// 可选 ffmpeg 过滤器链（卡顿排查用的实验开关）：默认必须为空。
func TestDefaultLeavesTheOptionalFFmpegFiltersUnset(t *testing.T) {
	if got := Default().MiPlayFFmpegAF; got != "" {
		t.Fatalf("Default().MiPlayFFmpegAF = %q, want empty", got)
	}
}

func TestLoadReadsOptionalFFmpegFilters(t *testing.T) {
	t.Setenv("MIPLAY_FFMPEG_AF", "  aresample=async=1:min_hard_comp=0.02  ")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "aresample=async=1:min_hard_comp=0.02"
	if cfg.MiPlayFFmpegAF != want {
		t.Fatalf("MiPlayFFmpegAF = %q, want %q", cfg.MiPlayFFmpegAF, want)
	}
}

func TestLoadTreatsBlankFFmpegFiltersAsUnset(t *testing.T) {
	t.Setenv("MIPLAY_FFMPEG_AF", "   ")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MiPlayFFmpegAF != "" {
		t.Fatalf("a blank MIPLAY_FFMPEG_AF must stay unset, got %q", cfg.MiPlayFFmpegAF)
	}
}
