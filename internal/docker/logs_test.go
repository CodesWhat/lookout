package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDecodeContainerLogStreamEmitsShortRawDataBeforeEOF(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	chunks := make(chan []byte, 8)
	done := make(chan error, 1)
	writeDone := make(chan error, 1)
	go func() {
		done <- DecodeContainerLogStream(reader, func(_ ContainerLogStream, payload []byte) error {
			chunks <- append([]byte(nil), payload...)
			return nil
		})
	}()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	go func() {
		_, err := writer.Write([]byte("tty\n"))
		writeDone <- err
	}()

	var got []byte
	for len(got) < len("tty\n") {
		select {
		case chunk := <-chunks:
			got = append(got, chunk...)
		case <-ctx.Done():
			t.Fatalf("short raw data was not emitted before EOF: got %q", got)
		}
	}
	if string(got) != "tty\n" {
		t.Fatalf("decoded raw data = %q, want %q", got, "tty\n")
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write raw log data: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("raw log writer remained blocked after data was emitted")
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("close raw log writer: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DecodeContainerLogStream: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("DecodeContainerLogStream did not stop after EOF")
	}
}

func TestDecodeContainerLogStreamPropagatesRawReaderAndEmitterErrors(t *testing.T) {
	t.Parallel()

	readErr := errors.New("raw read failed")
	if err := DecodeContainerLogStream(errReader{err: readErr}, func(ContainerLogStream, []byte) error {
		t.Fatal("emit called for a failed raw read")
		return nil
	}); !errors.Is(err, readErr) {
		t.Fatalf("DecodeContainerLogStream error = %v, want %v", err, readErr)
	}

	emitErr := errors.New("raw emit failed")
	var calls int
	err := DecodeContainerLogStream(strings.NewReader("raw tty log\n"), func(stream ContainerLogStream, payload []byte) error {
		calls++
		if stream != ContainerLogStdout {
			t.Fatalf("stream = %d, want stdout", stream)
		}
		if got := string(payload); got != "raw tty log\n" {
			t.Fatalf("payload = %q, want raw TTY log", got)
		}
		return emitErr
	})
	if !errors.Is(err, emitErr) {
		t.Fatalf("DecodeContainerLogStream error = %v, want %v", err, emitErr)
	}
	if calls != 1 {
		t.Fatalf("emit calls = %d, want 1", calls)
	}
}

func TestDecodeContainerLogStreamPropagatesMultiplexedEmitterError(t *testing.T) {
	t.Parallel()

	emitErr := errors.New("multiplexed emit failed")
	input := append(
		dockerLogFrame(byte(ContainerLogStderr), []byte("first\n")),
		dockerLogFrame(byte(ContainerLogStdout), []byte("must not be emitted\n"))...,
	)
	var calls int
	err := DecodeContainerLogStream(bytes.NewReader(input), func(stream ContainerLogStream, payload []byte) error {
		calls++
		if stream != ContainerLogStderr {
			t.Fatalf("stream = %d, want stderr", stream)
		}
		if got := string(payload); got != "first\n" {
			t.Fatalf("payload = %q, want first frame", got)
		}
		return emitErr
	})
	if !errors.Is(err, emitErr) {
		t.Fatalf("DecodeContainerLogStream error = %v, want %v", err, emitErr)
	}
	if calls != 1 {
		t.Fatalf("emit calls = %d, want decoder to stop after first error", calls)
	}
}

func dockerLogHeader(streamType byte, size uint32) []byte {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], size)
	return header
}

func TestDecodeContainerLogs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []byte
		want    []byte
		wantErr bool
	}{
		{
			name: "multiplexed stream is demuxed",
			in: append(
				dockerLogFrame(1, []byte("stdout\n")),
				dockerLogFrame(2, []byte("stderr\n"))...,
			),
			want: []byte("stdout\nstderr\n"),
		},
		{
			name: "raw tty text passes through unchanged",
			in:   []byte("hello world\nline two\n"),
			want: []byte("hello world\nline two\n"),
		},
		{
			name: "short input passes through unchanged",
			in:   []byte("short"),
			want: []byte("short"),
		},
		{
			name: "empty input returns empty output",
			in:   nil,
			want: []byte{},
		},
		{
			name: "raw stream with non header prefix passes through unchanged",
			in:   []byte{3, 0, 0, 0, 0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o'},
			want: []byte{3, 0, 0, 0, 0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o'},
		},
		{
			name: "truncated multiplexed frame returns partial bytes and error",
			in: append(
				dockerLogHeader(1, uint32(len("partial\n"))),
				[]byte("part")...,
			),
			want:    []byte("part"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := DecodeContainerLogs(bytes.NewReader(tc.in))
			if tc.wantErr && err == nil {
				t.Fatal("DecodeContainerLogs returned nil error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("DecodeContainerLogs returned error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("DecodeContainerLogs = %q, want %q", string(got), string(tc.want))
			}
		})
	}
}

type errReader struct {
	err error
}

func (r errReader) Read([]byte) (int, error) {
	return 0, r.err
}

// ---- decodeMultiplexedLogStream: maxLogFrameSize boundary ----

// TestDecodeMultiplexedLogStream_FrameAtCapIsNotSkipped exercises the exact
// boundary of the "size > maxLogFrameSize" check: a frame whose declared
// size equals maxLogFrameSize must be read and emitted like any other frame,
// not treated as oversized and skipped.
func TestDecodeMultiplexedLogStream_FrameAtCapIsNotSkipped(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte{'A'}, maxLogFrameSize)
	input := append(
		dockerLogFrame(byte(ContainerLogStdout), payload),
		dockerLogFrame(byte(ContainerLogStdout), []byte("second"))...,
	)

	var got [][]byte
	err := decodeMultiplexedLogStream(bytes.NewReader(input), func(_ ContainerLogStream, p []byte) error {
		got = append(got, append([]byte(nil), p...))
		return nil
	})
	if err != nil {
		t.Fatalf("decodeMultiplexedLogStream: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("emit calls = %d, want 2 (a frame at exactly maxLogFrameSize must not be skipped)", len(got))
	}
	if len(got[0]) != maxLogFrameSize {
		t.Fatalf("first payload length = %d, want %d", len(got[0]), maxLogFrameSize)
	}
	if string(got[1]) != "second" {
		t.Fatalf("second payload = %q, want %q", got[1], "second")
	}
}

// TestDecodeMultiplexedLogStream_OversizedFrameSkippedThenContinues exercises
// the negation boundary of the "io.CopyN(...) err != nil" check on its
// success side: when an oversized frame's payload is fully present, discard
// must succeed silently (no emit for it) and decoding must continue onto the
// next frame.
func TestDecodeMultiplexedLogStream_OversizedFrameSkippedThenContinues(t *testing.T) {
	t.Parallel()

	oversized := dockerLogFrame(byte(ContainerLogStdout), bytes.Repeat([]byte{'B'}, maxLogFrameSize+1))
	input := append(oversized, dockerLogFrame(byte(ContainerLogStderr), []byte("after"))...)

	type call struct {
		stream  ContainerLogStream
		payload []byte
	}
	var got []call
	err := decodeMultiplexedLogStream(bytes.NewReader(input), func(s ContainerLogStream, p []byte) error {
		got = append(got, call{s, append([]byte(nil), p...)})
		return nil
	})
	if err != nil {
		t.Fatalf("decodeMultiplexedLogStream: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("emit calls = %d, want 1 (oversized frame must be skipped, not emitted)", len(got))
	}
	if got[0].stream != ContainerLogStderr || string(got[0].payload) != "after" {
		t.Fatalf("emit = %+v, want stderr %q", got[0], "after")
	}
}

// TestDecodeMultiplexedLogStream_OversizedFrameTruncatedReturnsError is the
// failure-side mirror of the test above: when an oversized frame's payload
// is NOT fully present, the CopyN discard fails and decoding must return a
// wrapped error rather than silently continuing.
func TestDecodeMultiplexedLogStream_OversizedFrameTruncatedReturnsError(t *testing.T) {
	t.Parallel()

	header := dockerLogHeader(byte(ContainerLogStdout), maxLogFrameSize+100)
	// Only a few bytes of the claimed oversized payload actually follow.
	input := append(header, []byte("short")...)

	err := decodeMultiplexedLogStream(bytes.NewReader(input), func(ContainerLogStream, []byte) error {
		t.Fatal("emit called for a frame whose oversized payload was never fully available")
		return nil
	})
	if err == nil {
		t.Fatal("expected an error when discarding a truncated oversized frame")
	}
	if !strings.Contains(err.Error(), "skipping oversized log frame") {
		t.Fatalf("error = %v, want it to mention 'skipping oversized log frame'", err)
	}
}

// ---- decodeMultiplexedLogStream: zero-byte payload read ----

// TestDecodeMultiplexedLogStream_ZeroByteReadNotEmitted exercises the
// boundary of the "n > 0" check after io.ReadFull: when the payload read
// yields zero bytes (the reader ends immediately after the header), emit
// must not be called with an empty payload.
func TestDecodeMultiplexedLogStream_ZeroByteReadNotEmitted(t *testing.T) {
	t.Parallel()

	// Header claims a 5-byte payload, but no payload bytes follow at all.
	header := dockerLogHeader(byte(ContainerLogStdout), 5)

	err := decodeMultiplexedLogStream(bytes.NewReader(header), func(ContainerLogStream, []byte) error {
		t.Fatal("emit called despite zero payload bytes being read")
		return nil
	})
	if err == nil {
		t.Fatal("expected an error when the payload read yields zero bytes, got nil")
	}
}

// ---- looksMultiplexed ----

// TestLooksMultiplexed exercises each of the four ANDed conditions in
// isolation: for every condition, the other three are held true so that
// only the guard under test can turn the overall result from true to
// false. This kills each individual invert-&&-to-|| mutation, since an
// inverted operator would let the surrounding true operands paper over the
// one false operand this case targets.
func TestLooksMultiplexed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header []byte
		want   bool
	}{
		{name: "valid stdout frame header", header: []byte{1, 0, 0, 0, 0, 0, 0, 0}, want: true},
		{name: "valid stderr frame header", header: []byte{2, 0, 0, 0, 0, 0, 0, 0}, want: true},
		{name: "too short fails length check", header: []byte{1, 0, 0, 0, 0, 0, 0}, want: false},
		{name: "stream type out of range fails first byte check", header: []byte{3, 0, 0, 0, 0, 0, 0, 0}, want: false},
		{name: "byte 1 nonzero fails second byte check", header: []byte{1, 1, 0, 0, 0, 0, 0, 0}, want: false},
		{name: "byte 2 nonzero fails third byte check", header: []byte{1, 0, 1, 0, 0, 0, 0, 0}, want: false},
		{name: "byte 3 nonzero fails fourth byte check", header: []byte{1, 0, 0, 1, 0, 0, 0, 0}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := looksMultiplexed(tt.header); got != tt.want {
				t.Fatalf("looksMultiplexed(%v) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}
