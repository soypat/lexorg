package lexorg_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/soypat/lexorg"
)

// stream counts Read calls on a forward-only reader, and can be made to fail
// once a set number of bytes have gone out.
type stream struct {
	r        *strings.Reader
	reads    int
	failAt   int   // Bytes after which Read fails; 0 never fails.
	fail     error // The failure to report.
	produced int
}

func (s *stream) Read(b []byte) (int, error) {
	s.reads++
	if s.fail != nil && s.produced >= s.failAt {
		return 0, s.fail
	}
	if s.failAt > 0 && s.produced+len(b) > s.failAt {
		b = b[:s.failAt-s.produced] // Stop exactly on the failure point.
	}
	n, err := s.r.Read(b)
	s.produced += n
	return n, err
}

func newStream(data string) *stream { return &stream{r: strings.NewReader(data)} }

const streamData = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func TestStreamReaderAtSequential(t *testing.T) {
	// Buffer sizes around and below the read size, so a read lands inside one
	// span, across the seam, and past both.
	for _, bufsize := range []int{2, 4, 8, 16, 64, 256} {
		for _, readsize := range []int{1, 3, 8, 33} {
			var s lexorg.StreamReaderAt
			s.Reset(newStream(streamData), make([]byte, bufsize))

			var got []byte
			for {
				b := make([]byte, readsize)
				n, err := s.ReadAt(b, int64(len(got)))
				got = append(got, b[:n]...)
				if err == io.EOF {
					break
				} else if err != nil {
					t.Fatalf("buf %d read %d: ReadAt err=%v", bufsize, readsize, err)
				}
			}
			if string(got) != streamData {
				t.Errorf("buf %d read %d: read %q, want %q", bufsize, readsize, got, streamData)
			}
		}
	}
}

func TestStreamReaderAtRetained(t *testing.T) {
	// Halves of 8 bytes each, so the retained range spans at most 16.
	str := newStream(streamData)
	var s lexorg.StreamReaderAt
	s.Reset(str, make([]byte, 16))

	b := make([]byte, 4)
	if n, err := s.ReadAt(b, 0); n != 4 || err != nil {
		t.Fatalf("ReadAt(4, 0)=%d %v", n, err)
	}
	// One fill covered it, and the whole of that fill is now retained.
	if start, end := s.Retained(); start != 0 || end != 8 {
		t.Errorf("Retained()=[%d,%d), want [0,8)", start, end)
	}

	// Reading back inside the retained range costs nothing.
	reads := str.reads
	for off := int64(0); off <= 4; off++ {
		if n, err := s.ReadAt(b, off); n != 4 || err != nil {
			t.Fatalf("reread at %d=%d %v", off, n, err)
		} else if string(b) != streamData[off:off+4] {
			t.Errorf("reread at %d=%q, want %q", off, b, streamData[off:off+4])
		}
	}
	if str.reads != reads {
		t.Errorf("rereading retained bytes cost %d reads, want 0", str.reads-reads)
	}

	// Reading forward flips the buffers; both spans stay retained and abut.
	if _, err := s.ReadAt(b, 12); err != nil {
		t.Fatalf("ReadAt(4, 12) err=%v", err)
	}
	if start, end := s.Retained(); start != 0 || end != 16 {
		t.Errorf("Retained()=[%d,%d), want [0,16) after the flip", start, end)
	}

	// A read crossing the seam between the two buffers comes out whole.
	seam := make([]byte, 6)
	if n, err := s.ReadAt(seam, 5); n != 6 || err != nil {
		t.Fatalf("seam ReadAt=%d %v", n, err)
	} else if string(seam) != streamData[5:11] {
		t.Errorf("seam ReadAt=%q, want %q", seam, streamData[5:11])
	}

	// One more flip retires the oldest span.
	if _, err := s.ReadAt(b, 20); err != nil {
		t.Fatalf("ReadAt(4, 20) err=%v", err)
	}
	if start, end := s.Retained(); start != 8 || end != 24 {
		t.Errorf("Retained()=[%d,%d), want [8,24)", start, end)
	}
}

func TestStreamReaderAtRewound(t *testing.T) {
	var s lexorg.StreamReaderAt
	s.Reset(newStream(streamData), make([]byte, 16))

	b := make([]byte, 4)
	if _, err := s.ReadAt(b, 20); err != nil {
		t.Fatalf("ReadAt(4, 20) err=%v", err)
	}
	start, _ := s.Retained()
	if start == 0 {
		t.Fatal("nothing was retired, so there is no rewind to test")
	}

	// An offset behind the retained range is gone for good.
	for _, off := range []int64{0, start - 1} {
		if n, err := s.ReadAt(b, off); !errors.Is(err, lexorg.ErrRewound) || n != 0 {
			t.Errorf("ReadAt at %d (retained from %d)=%d %v, want 0 %v",
				off, start, n, err, lexorg.ErrRewound)
		}
	}
	// The oldest retained offset still reads.
	if n, err := s.ReadAt(b, start); n != 4 || err != nil {
		t.Errorf("ReadAt at the oldest retained offset %d=%d %v", start, n, err)
	}

	// A negative offset precedes everything and reports the same.
	if _, err := s.ReadAt(b, -1); !errors.Is(err, lexorg.ErrRewound) {
		t.Errorf("ReadAt(-1) err=%v, want %v", err, lexorg.ErrRewound)
	}
}

func TestStreamReaderAtOversized(t *testing.T) {
	// A request larger than both buffers is served by consuming forward, so the
	// retained range never has to hold it all at once.
	var s lexorg.StreamReaderAt
	s.Reset(newStream(streamData), make([]byte, 8))

	b := make([]byte, len(streamData))
	n, err := s.ReadAt(b, 0)
	if n != len(streamData) || err != nil {
		t.Fatalf("oversized ReadAt=%d %v, want %d nil", n, err, len(streamData))
	}
	if string(b) != streamData {
		t.Errorf("oversized ReadAt=%q, want %q", b, streamData)
	}
	// Only the last two spans survive it.
	if start, end := s.Retained(); end-start > 8 || end != int64(len(streamData)) {
		t.Errorf("Retained()=[%d,%d) after an oversized read", start, end)
	}
}

func TestStreamReaderAtEOF(t *testing.T) {
	const data = "0123456789"
	var s lexorg.StreamReaderAt
	s.Reset(newStream(data), make([]byte, 16))

	// A read overrunning the end returns what there was, with io.EOF.
	b := make([]byte, 8)
	if n, err := s.ReadAt(b, 6); n != 4 || err != io.EOF {
		t.Fatalf("ReadAt past the end=%d %v, want 4 io.EOF", n, err)
	} else if string(b[:n]) != data[6:] {
		t.Errorf("ReadAt past the end=%q, want %q", b[:n], data[6:])
	}
	if !errors.Is(s.Err(), io.EOF) {
		t.Errorf("Err()=%v after the end, want io.EOF", s.Err())
	}

	// The end is sticky, but the bytes still retained keep reading.
	if n, err := s.ReadAt(b, 10); n != 0 || err != io.EOF {
		t.Errorf("ReadAt wholly past the end=%d %v, want 0 io.EOF", n, err)
	}
	if n, err := s.ReadAt(b[:4], 0); n != 4 || err != nil {
		t.Errorf("retained ReadAt after the end=%d %v, want 4 nil", n, err)
	}
}

func TestStreamReaderAtErr(t *testing.T) {
	errRead := errors.New("stream failed")
	str := newStream(streamData)
	str.failAt, str.fail = 12, errRead

	var s lexorg.StreamReaderAt
	s.Reset(str, make([]byte, 16))

	// The failure surfaces once the stream has handed over what preceded it.
	b := make([]byte, 20)
	n, err := s.ReadAt(b, 0)
	if !errors.Is(err, errRead) {
		t.Fatalf("ReadAt over a failing stream err=%v, want %v", err, errRead)
	}
	if n != 12 || string(b[:n]) != streamData[:12] {
		t.Errorf("ReadAt over a failing stream=%d %q, want 12 %q", n, b[:n], streamData[:12])
	}

	// It is sticky: a stream cannot be retried.
	reads := str.reads
	if _, err := s.ReadAt(b, 12); !errors.Is(err, errRead) {
		t.Errorf("second ReadAt err=%v, want the latched %v", err, errRead)
	}
	if str.reads != reads {
		t.Errorf("a latched failure cost %d reads, want 0", str.reads-reads)
	}
	// Bytes read before the failure are still retained.
	if n, err := s.ReadAt(b[:8], 0); n != 8 || err != nil {
		t.Errorf("retained ReadAt after a failure=%d %v, want 8 nil", n, err)
	}
}

// idle hands back no bytes and no error for the first empty reads of every
// fill, which io.Reader discourages without forbidding.
type idle struct {
	r     io.Reader
	empty int // Empty reads to return before each real one.
	left  int
	reads int
}

func (i *idle) Read(b []byte) (int, error) {
	i.reads++
	if i.left > 0 {
		i.left--
		return 0, nil
	}
	i.left = i.empty
	if i.r == nil {
		return 0, nil // Never makes progress at all.
	}
	return i.r.Read(b)
}

func TestStreamReaderAtNoProgress(t *testing.T) {
	// With no Yield to say how long to wait, a read that makes no progress
	// fails at once rather than spin on a count this package chose.
	stuck := &idle{}
	var s lexorg.StreamReaderAt
	s.Reset(stuck, make([]byte, 16))

	n, err := s.ReadAt(make([]byte, 4), 0)
	if !errors.Is(err, io.ErrNoProgress) || n != 0 {
		t.Fatalf("ReadAt on a stuck reader=%d %v, want 0 %v", n, err, io.ErrNoProgress)
	}
	if stuck.reads != 1 {
		t.Errorf("a stuck reader was polled %d times, want 1", stuck.reads)
	}
	// Giving up latches, so a second call does not poll it again either.
	reads := stuck.reads
	if _, err := s.ReadAt(make([]byte, 4), 0); !errors.Is(err, io.ErrNoProgress) {
		t.Errorf("second ReadAt err=%v, want the latched %v", err, io.ErrNoProgress)
	}
	if stuck.reads != reads {
		t.Errorf("a latched failure polled the reader %d more times", stuck.reads-reads)
	}
}

func TestStreamReaderAtEmptyReadsRecover(t *testing.T) {
	// A reader that stalls between real reads gets through only because Yield
	// keeps asking for it. Without one the same stream fails on the first stall.
	slow := &idle{r: strings.NewReader(streamData), empty: 3}
	var bare lexorg.StreamReaderAt
	bare.Reset(slow, make([]byte, 16))
	// Far enough to outrun the one fill that lands before the first stall.
	if n, err := bare.ReadAt(make([]byte, len(streamData)), 0); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("stalling reader with no Yield=%d %v, want %v", n, err, io.ErrNoProgress)
	}

	slow = &idle{r: strings.NewReader(streamData), empty: 3}
	var s lexorg.StreamReaderAt
	s.Yield = func(uint) bool { return true }
	s.Reset(slow, make([]byte, 16))

	b := make([]byte, len(streamData))
	n, err := s.ReadAt(b, 0)
	if n != len(streamData) || err != nil {
		t.Fatalf("ReadAt over a slow reader=%d %v, want %d nil", n, err, len(streamData))
	}
	if string(b) != streamData {
		t.Errorf("ReadAt over a slow reader=%q, want %q", b, streamData)
	}
}

func TestStreamReaderAtYield(t *testing.T) {
	// Yield sees the run length, starting at 1 and restarting once bytes come
	// through, so a backoff keyed off it never carries across progress.
	slow := &idle{r: strings.NewReader(streamData), empty: 3}
	var s lexorg.StreamReaderAt
	var seen []uint
	s.Yield = func(consecutiveYields uint) bool {
		seen = append(seen, consecutiveYields)
		return true
	}
	s.Reset(slow, make([]byte, 16))

	b := make([]byte, len(streamData))
	if n, err := s.ReadAt(b, 0); n != len(streamData) || err != nil {
		t.Fatalf("ReadAt over a slow reader=%d %v, want %d nil", n, err, len(streamData))
	}
	if string(b) != streamData {
		t.Fatalf("ReadAt over a slow reader=%q, want %q", b, streamData)
	}
	if len(seen) == 0 {
		t.Fatal("Yield was never called over a reader that stalls")
	}
	for i, got := range seen {
		if want := uint(i%3 + 1); got != want {
			t.Fatalf("Yield call %d saw %d consecutive, want %d (run %v)", i, got, want, seen)
		}
	}
}

func TestStreamReaderAtYieldHalts(t *testing.T) {
	// Returning false gives up on the stream, however early.
	const giveUpAfter = 3
	stuck := &idle{}
	var s lexorg.StreamReaderAt
	calls := uint(0)
	s.Yield = func(consecutiveYields uint) bool {
		calls = consecutiveYields
		return consecutiveYields < giveUpAfter
	}
	s.Reset(stuck, make([]byte, 16))

	if n, err := s.ReadAt(make([]byte, 4), 0); !errors.Is(err, io.ErrNoProgress) || n != 0 {
		t.Fatalf("ReadAt when Yield halts=%d %v, want 0 %v", n, err, io.ErrNoProgress)
	}
	if calls != giveUpAfter {
		t.Errorf("Yield stopped at %d, want %d", calls, giveUpAfter)
	}
	if stuck.reads != giveUpAfter {
		t.Errorf("a halting Yield let the reader be polled %d times, want %d",
			stuck.reads, giveUpAfter)
	}

	// Halting latches like any other failure.
	reads := stuck.reads
	if _, err := s.ReadAt(make([]byte, 4), 0); !errors.Is(err, io.ErrNoProgress) {
		t.Errorf("second ReadAt err=%v, want the latched %v", err, io.ErrNoProgress)
	}
	if stuck.reads != reads {
		t.Errorf("a latched failure polled the reader %d more times", stuck.reads-reads)
	}
}

func TestStreamReaderAtYieldSurvivesReset(t *testing.T) {
	// Yield configures s, so rebinding the stream must not clear it.
	var s lexorg.StreamReaderAt
	calls := 0
	s.Yield = func(uint) bool { calls++; return false }
	s.Reset(&idle{}, make([]byte, 16))
	if _, err := s.ReadAt(make([]byte, 4), 0); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("ReadAt err=%v, want %v", err, io.ErrNoProgress)
	}
	s.Reset(&idle{}, nil)
	if _, err := s.ReadAt(make([]byte, 4), 0); !errors.Is(err, io.ErrNoProgress) {
		t.Errorf("ReadAt after Reset err=%v, want %v", err, io.ErrNoProgress)
	}
	if calls != 2 {
		t.Errorf("Yield ran %d times across the Reset, want 2", calls)
	}
}

func TestStreamReaderAtUninitialized(t *testing.T) {
	var zero lexorg.StreamReaderAt
	if n, err := zero.ReadAt(make([]byte, 4), 0); !errors.Is(err, lexorg.ErrUninitialized) || n != 0 {
		t.Errorf("zero StreamReaderAt ReadAt=%d %v, want 0 %v", n, err, lexorg.ErrUninitialized)
	}

	// A buffer too small to halve leaves nothing to ping-pong between.
	var s lexorg.StreamReaderAt
	s.Reset(newStream(streamData), make([]byte, 1))
	if _, err := s.ReadAt(make([]byte, 4), 0); !errors.Is(err, lexorg.ErrUninitialized) {
		t.Errorf("unhalvable buffer err=%v, want %v", err, lexorg.ErrUninitialized)
	}

	// Reset restarts offsets at zero over a fresh stream, keeping the buffer.
	s.Reset(newStream(streamData), make([]byte, 16))
	b := make([]byte, 4)
	if n, err := s.ReadAt(b, 0); n != 4 || err != nil || string(b) != streamData[:4] {
		t.Fatalf("ReadAt after Reset=%d %q %v", n, b, err)
	}
	s.Reset(newStream(streamData), nil)
	if start, end := s.Retained(); start != 0 || end != 0 {
		t.Errorf("Reset left Retained()=[%d,%d), want [0,0)", start, end)
	}
	if n, err := s.ReadAt(b, 0); n != 4 || err != nil || string(b) != streamData[:4] {
		t.Errorf("ReadAt after Reset(nil buf)=%d %q %v", n, b, err)
	}
}

func TestStreamReaderAtEmptyRead(t *testing.T) {
	str := newStream(streamData)
	var s lexorg.StreamReaderAt
	s.Reset(str, make([]byte, 16))

	if n, err := s.ReadAt(nil, 0); n != 0 || err != nil {
		t.Errorf("ReadAt(nil, 0)=%d %v, want 0 nil", n, err)
	}
	if str.reads != 0 {
		t.Errorf("an empty read cost %d reads, want 0", str.reads)
	}
}

func TestStreamReaderAtWindow(t *testing.T) {
	// The point of the adapter: a Window over a stream, walked forward.
	var s lexorg.StreamReaderAt
	s.Reset(newStream(streamData), make([]byte, 32))

	var wr lexorg.WindowReader
	wr.Reset(&s, make([]byte, 8), 0)

	var got []byte
	for {
		b, err := wr.ReadByte()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("ReadByte err=%v", err)
		}
		got = append(got, b)
	}
	if string(got) != streamData {
		t.Errorf("WindowReader over a stream walked %q, want %q", got, streamData)
	}

	// Bytes the window already holds cost the stream nothing, which is the
	// property the adapter has to preserve for a Window to be worth layering
	// over a stream at all.
	str := newStream(streamData)
	s.Reset(str, nil)
	wr.Reset(&s, nil, 0)
	if _, err := wr.ReadByte(); err != nil {
		t.Fatalf("ReadByte err=%v", err)
	}
	reads := str.reads
	for i := 1; i < 8; i++ { // The rest of the window's 8 byte fill.
		if b, err := wr.ReadByte(); err != nil || b != streamData[i] {
			t.Fatalf("ReadByte at %d=%q %v, want %q", i, b, err, streamData[i])
		}
	}
	if str.reads != reads {
		t.Errorf("walking bytes the window held cost %d stream reads, want 0", str.reads-reads)
	}
}
