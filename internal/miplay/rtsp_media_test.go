package miplay

import "testing"

// MiUI 会把媒体帧（0x24 帧头）混进 RTSP 控制腿。解码器必须识别并跳过，
// 而不是把这些二进制当 RTSP 文本解析后撞到 >0x7F 字节报错断开。
// 场景来自 10-04 真机实测：首字节 0x24、第4字节 0x80，原实现报
// “RTSP headers are not ASCII (offset 4/…)”，表现为能连上但全程无声。

func mediaFrame(payload []byte) []byte {
	frame := make([]byte, MediaHeaderSize+len(payload))
	frame[0] = MediaMagic
	frame[1] = byte(len(payload) >> 16)
	frame[2] = byte(len(payload) >> 8)
	frame[3] = byte(len(payload))
	copy(frame[MediaHeaderSize:], payload)
	return frame
}

const rtspOptions = "OPTIONS rtsp://src RTSP/1.0\r\nCSeq: 1\r\n\r\n"

func TestFeedSkipsInterleavedMediaFrame(t *testing.T) {
	d := NewRTSPDecoder()
	payload := []byte{0x80, 0xa1, 0x00, 0x11, 0x00, 0x00, 0xb0, 0x0d}
	data := append(mediaFrame(payload), []byte(rtspOptions)...)

	messages, err := d.Feed(data)
	if err != nil {
		t.Fatalf("Feed returned error for interleaved media: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("want exactly 1 RTSP message after the media frame, got %d", len(messages))
	}
	if messages[0].StartLine != "OPTIONS rtsp://src RTSP/1.0" {
		t.Fatalf("start line = %q", messages[0].StartLine)
	}
}

func TestFeedSkipsMultipleMediaFrames(t *testing.T) {
	d := NewRTSPDecoder()
	var data []byte
	for i := 0; i < 5; i++ {
		data = append(data, mediaFrame([]byte{byte(i), 0xff, 0x80})...)
	}
	data = append(data, []byte(rtspOptions)...)

	messages, err := d.Feed(data)
	if err != nil {
		t.Fatalf("Feed returned error: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(messages))
	}
}

func TestFeedWaitsForPartialMediaFrame(t *testing.T) {
	d := NewRTSPDecoder()
	payload := []byte{0x80, 0xa1, 0x00, 0x11, 0x00, 0x00}
	frame := mediaFrame(payload)

	// 只送前半截：不能报错，也不能产出消息，要安静等下一批。
	if _, err := d.Feed(frame[:5]); err != nil {
		t.Fatalf("partial frame must not error, got %v", err)
	}
	messages, err := d.Feed(append(frame[5:], []byte(rtspOptions)...))
	if err != nil {
		t.Fatalf("Feed returned error after completion: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("want 1 message once frame completed, got %d", len(messages))
	}
}

func TestFeedRejectsZeroLengthMediaFrame(t *testing.T) {
	d := NewRTSPDecoder()
	// 0x24 帧头但长度=0（非法），后面跟一段完整的 RTSP 头让解析器真正走到
	// ASCII 校验：buffer 首位非 0x24 之前会先命中 indexOfDoubleCRLF，
	// 若头部含 >0x7F 字节应报错，而不是静默接受。
	bad := append([]byte{MediaMagic, 0x00, 0x00, 0x00}, []byte("X rtsp://src RTSP/1.0\r\nCSeq: 1\r\nX-\x80: 1\r\n\r\n")...)
	if _, err := d.Feed(bad); err == nil {
		t.Fatal("want error for zero-length media frame followed by non-ASCII header, got nil")
	}
}

func TestFeedPureRTSPUnaffected(t *testing.T) {
	d := NewRTSPDecoder()
	messages, err := d.Feed([]byte(rtspOptions))
	if err != nil || len(messages) != 1 {
		t.Fatalf("pure RTSP path regressed: err=%v messages=%d", err, len(messages))
	}
}

// 回归：整帧消费后 buffer 恰好变空。跳过媒体帧会让缓冲区被清空，若循环下一轮
// 直接索引 d.buffer[0] 就会切片越界 panic —— 这里把“空 buffer”钉死。
func TestFeedMediaFrameLeavesEmptyBuffer(t *testing.T) {
	d := NewRTSPDecoder()
	// 只喂一个完整媒体帧，后面不跟任何数据 → 缓冲区被清空
	messages, err := d.Feed(mediaFrame([]byte{0x80, 0xa1, 0x00, 0x11}))
	if err != nil {
		t.Fatalf("lone media frame returned error: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("want no RTSP messages after a lone media frame, got %d", len(messages))
	}
	// 清空之后解码器必须还能继续用（不能因为空 buffer 留下坏状态）
	messages, err = d.Feed([]byte(rtspOptions))
	if err != nil || len(messages) != 1 {
		t.Fatalf("decoder unusable after draining to empty: err=%v messages=%d", err, len(messages))
	}
}

// 回归：0x24 之后不够一整帧头（短 buffer）时，必须安静等待，不得越界。
func TestFeedShortMediaFrameHeader(t *testing.T) {
	d := NewRTSPDecoder()
	if messages, err := d.Feed([]byte{MediaMagic}); err != nil || len(messages) != 0 {
		t.Fatalf("lone magic byte: err=%v messages=%d", err, len(messages))
	}
	if messages, err := d.Feed([]byte{MediaMagic, 0x00, 0x00, 0x00}); err != nil || len(messages) != 0 {
		t.Fatalf("zero-length frame header: err=%v messages=%d", err, len(messages))
	}
}
