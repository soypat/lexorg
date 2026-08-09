package lexorg_test

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"unicode/utf8"
	"unsafe"

	"github.com/soypat/lexorg"
)

// counter counts ReadAt calls, which is what the Window exists to avoid.
type counter struct {
	r     io.ReaderAt
	reads int
	fail  error // When set, every ReadAt fails with it.
}

func (c *counter) ReadAt(b []byte, off int64) (int, error) {
	c.reads++
	if c.fail != nil {
		return 0, c.fail
	}
	return c.r.ReadAt(b, off)
}

func newCounter(data string) *counter {
	return &counter{r: bytes.NewReader([]byte(data))}
}

// byteAt fails the test unless the window yields want at off.
func byteAt(t *testing.T, w *lexorg.Window, off int64, want byte) {
	t.Helper()
	got, ok := w.ByteAt(off)
	if !ok {
		t.Fatalf("ByteAt(%d) not ok, err=%v", off, w.Err())
	} else if got != want {
		t.Fatalf("ByteAt(%d)=%q want %q", off, got, want)
	}
}

func TestWindowSize(t *testing.T) {
	// The point of the layout is that base+buf+r+err pack without padding and
	// without the redundant end and n fields.
	const want = 8 + 24 + 2*16
	if got := unsafe.Sizeof(lexorg.Window{}); got != want {
		t.Errorf("unsafe.Sizeof(Window{})=%d want %d", got, want)
	}
}

func TestWindowResident(t *testing.T) {
	c := newCounter("0123456789abcdef")
	var w lexorg.Window
	w.Reset(c, make([]byte, 8))

	byteAt(t, &w, 0, '0')
	if c.reads != 1 {
		t.Fatalf("first ByteAt did %d reads, want 1", c.reads)
	}
	// Every offset the fill covered must come out of the buffer.
	for off := int64(0); off < 8; off++ {
		byteAt(t, &w, off, "0123456789abcdef"[off])
	}
	if c.reads != 1 {
		t.Errorf("resident reads cost %d reads, want 1", c.reads)
	}
	// Past the resident span: one refill, then resident again.
	byteAt(t, &w, 8, '8')
	if c.reads != 2 {
		t.Fatalf("refill did %d reads, want 2", c.reads)
	}
	byteAt(t, &w, 15, 'f')
	if c.reads != 2 {
		t.Errorf("read inside refilled window cost %d reads, want 2", c.reads)
	}
	// Backwards out of the window refills too.
	byteAt(t, &w, 0, '0')
	if c.reads != 3 {
		t.Errorf("backward jump did %d reads, want 3", c.reads)
	}
}

func TestWindowResetKeepsBytes(t *testing.T) {
	const data = "0123456789abcdef"
	buf := make([]byte, 8)
	c := newCounter(data)

	var w lexorg.Window
	w.Reset(c, buf)
	byteAt(t, &w, 0, '0')

	// Same reader, same buffer: the resident bytes still describe the file.
	w.Reset(c, buf)
	byteAt(t, &w, 3, '3')
	if c.reads != 1 {
		t.Errorf("Reset(same, same) cost %d reads, want 1", c.reads)
	}

	// Same reader, nil buffer: still the same file, so the bytes stand.
	w.Reset(c, nil)
	byteAt(t, &w, 3, '3')
	if c.reads != 1 {
		t.Errorf("Reset(same, nil) cost %d reads, want 1", c.reads)
	}

	// Different reader: the bytes describe the old file, so they must go. A nil
	// buf still keeps the fill buffer, which is the only way to have one.
	c2 := newCounter("FEDCBA9876543210")
	w.Reset(c2, nil)
	if w.BufferCapacity() != len(buf) {
		t.Fatalf("Reset(other, nil) left cap(buf)=%d, want %d", w.BufferCapacity(), len(buf))
	}
	byteAt(t, &w, 3, 'C')
	if c2.reads != 1 {
		t.Errorf("Reset(other, nil) cost %d reads, want 1", c2.reads)
	}

	// Different buffer over the same reader: same reasoning.
	before := c2.reads
	w.Reset(c2, make([]byte, 8))
	byteAt(t, &w, 3, 'C')
	if c2.reads != before+1 {
		t.Errorf("Reset(same, other buf) cost %d reads, want %d", c2.reads-before, 1)
	}
}

func TestWindowEOF(t *testing.T) {
	c := newCounter("abc")
	var w lexorg.Window
	w.Reset(c, make([]byte, 8))

	byteAt(t, &w, 0, 'a')
	if !w.EOF() {
		t.Error("EOF()=false after a fill that ran off the end")
	}

	// At or past the end: the recorded EOF answers without a read.
	reads := c.reads
	for _, off := range []int64{3, 4, 100} {
		if _, ok := w.ByteAt(off); ok {
			t.Errorf("ByteAt(%d) ok past end of file", off)
		}
	}
	if c.reads != reads {
		t.Errorf("reads past end of file cost %d reads, want 0", c.reads-reads)
	}

	// Below base, EOF says nothing, so the window reads.
	w.Drop()
	if _, ok := w.ByteAt(2); !ok {
		t.Fatal("ByteAt(2) not ok after Drop")
	}
	if _, ok := w.ByteAt(1); !ok {
		t.Error("ByteAt(1) not ok: EOF at a higher offset must not bar lower ones")
	}
}

// TestWindowFillEmptyKeepsBytes covers the fill that comes back with nothing:
// no bytes were read, so the resident ones still describe the file and must
// survive. Reset clears the recorded EOF, which is what lets such a fill be
// attempted at all.
func TestWindowFillEmptyKeepsBytes(t *testing.T) {
	const data = "abc"
	c := newCounter(data)
	var w lexorg.Window
	w.Reset(c, make([]byte, 2*len(data)))

	byteAt(t, &w, 0, data[0])
	want, wantBase := w.Buffer()
	if len(want) != len(data) || wantBase != 0 {
		t.Fatalf("setup left %d bytes @%d, want %d @0", len(want), wantBase, len(data))
	}

	// Same reader, so the bytes stay; the recorded EOF does not.
	w.Reset(c, nil)
	if _, ok := w.Fill(int64(len(data))); ok {
		t.Fatal("Fill past the end of the file reported bytes")
	}
	got, base := w.Buffer()
	if base != wantBase || string(got) != string(want) {
		t.Errorf("empty fill left %q @%d, want %q @%d", got, base, want, wantBase)
	}
	// The bytes are not merely present, they are still reachable without a read.
	reads := c.reads
	byteAt(t, &w, int64(len(data))-1, data[len(data)-1])
	if c.reads != reads {
		t.Errorf("reading a kept byte cost %d reads, want 0", c.reads-reads)
	}
}

func TestWindowErr(t *testing.T) {
	errRead := errors.New("read failed")
	c := newCounter("0123456789abcdef")
	var w lexorg.Window
	w.Reset(c, make([]byte, 8))

	c.fail = errRead
	if _, ok := w.ByteAt(8); ok {
		t.Fatal("ByteAt ok on a failing reader")
	} else if !errors.Is(w.Err(), errRead) {
		t.Fatalf("Err()=%v, want %v", w.Err(), errRead)
	}
	if w.EOF() {
		t.Error("EOF()=true on a read failure")
	}

	// At or past the failed offset, no read is attempted.
	reads := c.reads
	if _, ok := w.ByteAt(9); ok {
		t.Error("ByteAt ok at an offset past the failure")
	}
	if c.reads != reads {
		t.Errorf("read past the failure cost %d reads, want 0", c.reads-reads)
	}

	// The error describes the last fill, so a fill that succeeds clears it.
	c.fail = nil
	byteAt(t, &w, 0, '0')
	if err := w.Err(); err != nil {
		t.Errorf("Err()=%v after a successful fill, want nil", err)
	}
}

func TestWindowUninitialized(t *testing.T) {
	var zero lexorg.Window
	if _, ok := zero.ByteAt(0); ok {
		t.Error("zero Window yielded a byte")
	} else if !errors.Is(zero.Err(), lexorg.ErrUninitialized) {
		t.Errorf("zero Window Err()=%v, want %v", zero.Err(), lexorg.ErrUninitialized)
	}

	// A reader but no buffer: nothing to read into.
	var w lexorg.Window
	w.Reset(newCounter("abc"), nil)
	if _, ok := w.ByteAt(0); ok {
		t.Error("Window with no buffer yielded a byte")
	} else if !errors.Is(w.Err(), lexorg.ErrUninitialized) {
		t.Errorf("bufferless Err()=%v, want %v", w.Err(), lexorg.ErrUninitialized)
	}

	// A Reset supplying one makes it usable, and clears the error.
	w.Reset(newCounter("abc"), make([]byte, 8))
	byteAt(t, &w, 0, 'a')
}

func TestWindowNegativeOffset(t *testing.T) {
	c := newCounter("abc")
	var w lexorg.Window
	w.Reset(c, make([]byte, 8))

	if _, ok := w.ByteAt(-1); ok {
		t.Error("ByteAt(-1) ok")
	}
	if c.reads != 0 {
		t.Errorf("ByteAt(-1) cost %d reads, want 0", c.reads)
	}
	if err := w.Err(); err != nil {
		t.Errorf("Err()=%v after a negative offset, want nil", err)
	}
	byteAt(t, &w, 0, 'a')
}

func TestWindowDrop(t *testing.T) {
	c := newCounter("0123456789abcdef")
	var w lexorg.Window
	w.Reset(c, make([]byte, 8))

	byteAt(t, &w, 0, '0')
	byteAt(t, &w, 1, '1')
	if c.reads != 1 {
		t.Fatalf("setup did %d reads, want 1", c.reads)
	}
	w.Drop()
	byteAt(t, &w, 1, '1')
	if c.reads != 2 {
		t.Errorf("Drop then ByteAt did %d reads, want 2", c.reads)
	}
	if w.BufferCapacity() != 8 {
		t.Errorf("Drop left cap(buf)=%d, want 8: the fill buffer must survive", w.BufferCapacity())
	}
}

// resident returns a Window over data holding the fill that starts at off, and
// the reader with its count zeroed so a test measures only what follows.
func resident(t *testing.T, data string, bufsize int, off int64) (*lexorg.Window, *counter) {
	t.Helper()
	c := newCounter(data)
	w := new(lexorg.Window)
	w.Reset(c, make([]byte, bufsize))
	w.Fill(off)
	buf, base := w.Buffer()
	if base != off || len(buf) == 0 {
		t.Fatalf("setup fill at %d left base=%d len(buf)=%d", off, base, len(buf))
	}
	c.reads = 0
	return w, c
}

// unmoved fails the test unless the window still holds what it did, which is
// ReadAt's whole promise: a bulk read must not evict the cursor's bytes.
func unmoved(t *testing.T, w *lexorg.Window, buf string, base int64, err error) {
	t.Helper()
	gotbuf, gotbase := w.Buffer()
	if string(gotbuf) != buf || gotbase != base {
		t.Errorf("ReadAt moved the window to base=%d %q, want base=%d %q",
			gotbase, gotbuf, base, buf)
	}
	if w.Err() != err {
		t.Errorf("ReadAt changed Err() to %v, want %v", w.Err(), err)
	}
}

func TestWindowReadAt(t *testing.T) {
	// The window holds [4,12) of a 16 byte file, so a request can miss it, sit
	// inside it, or overhang either end.
	const data = "0123456789abcdef"
	const (
		bufsize = 8
		base    = 4
	)
	resbuf := data[base : base+bufsize]

	for _, test := range []struct {
		name  string
		off   int64
		n     int
		reads int // Reads the request should cost.
	}{
		{name: "resident", off: 4, n: 4, reads: 0},
		{name: "resident exact", off: 4, n: 8, reads: 0},
		{name: "resident tail", off: 10, n: 2, reads: 0},
		{name: "before window", off: 0, n: 4, reads: 1},
		{name: "after window", off: 12, n: 4, reads: 1},
		{name: "overhangs head", off: 2, n: 6, reads: 1},
		{name: "overhangs tail", off: 8, n: 6, reads: 1},
		{name: "straddles window", off: 0, n: 16, reads: 2},
		{name: "abuts head", off: 0, n: 6, reads: 1},
		{name: "abuts tail", off: 11, n: 3, reads: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, c := resident(t, data, bufsize, base)
			b := make([]byte, test.n)
			n, err := w.ReadAt(b, test.off)
			want := data[test.off : int(test.off)+test.n]
			if err != nil {
				t.Fatalf("ReadAt(%d bytes, %d) err=%v", test.n, test.off, err)
			} else if n != test.n {
				t.Fatalf("ReadAt(%d bytes, %d)=%d bytes", test.n, test.off, n)
			} else if string(b) != want {
				t.Errorf("ReadAt(%d bytes, %d)=%q want %q", test.n, test.off, b, want)
			}
			if c.reads != test.reads {
				t.Errorf("ReadAt cost %d reads, want %d", c.reads, test.reads)
			}
			unmoved(t, w, resbuf, base, nil)
		})
	}
}

func TestWindowReadAtEOF(t *testing.T) {
	// A 10 byte file with an 8 byte buffer: the fill at 4 runs off the end, so
	// the window holds [4,10) and knows the file stops there.
	const data = "0123456789"
	w, c := resident(t, data, 8, 4)
	if !w.EOF() {
		t.Fatal("setup fill did not reach the end of the file")
	}

	// Overhanging the tail of a window that ended the file: the resident bytes
	// come back, and the part past the end needs no read to rule out.
	b := make([]byte, 8)
	n, err := w.ReadAt(b, 6)
	if err != io.EOF {
		t.Errorf("ReadAt past the end err=%v, want io.EOF", err)
	} else if n != 4 || string(b[:n]) != "6789" {
		t.Errorf("ReadAt past the end=%d %q, want 4 %q", n, b[:n], "6789")
	}
	if c.reads != 0 {
		t.Errorf("ReadAt past a known end cost %d reads, want 0", c.reads)
	}
	unmoved(t, w, data[4:], 4, io.EOF)

	// Wholly past the window: nothing aliases, so the reader is asked and it is
	// the one that reports the end.
	if n, err := w.ReadAt(b, 10); n != 0 || err != io.EOF {
		t.Errorf("ReadAt wholly past the end=%d %v, want 0 io.EOF", n, err)
	}
	unmoved(t, w, data[4:], 4, io.EOF)

	// A request the window covers entirely is served whole, end of file or not.
	if n, err := w.ReadAt(b[:6], 4); n != 6 || err != nil {
		t.Errorf("resident ReadAt=%d %v, want 6 nil", n, err)
	}
}

func TestWindowReadAtShortTail(t *testing.T) {
	// The window ends mid-file, so the tail read is what finds the end.
	const data = "0123456789abcdef"
	w, c := resident(t, data, 8, 4)

	b := make([]byte, 12)
	n, err := w.ReadAt(b, 8)
	if err != io.EOF {
		t.Errorf("ReadAt overrunning the file err=%v, want io.EOF", err)
	} else if n != 8 || string(b[:n]) != "89abcdef" {
		t.Errorf("ReadAt overrunning the file=%d %q, want 8 %q", n, b[:n], "89abcdef")
	}
	if c.reads != 1 {
		t.Errorf("ReadAt cost %d reads, want 1", c.reads)
	}
	unmoved(t, w, data[4:12], 4, nil)
}

func TestWindowReadAtErr(t *testing.T) {
	errRead := errors.New("read failed")
	const data = "0123456789abcdef"

	// A failure reading the head must not report the resident bytes behind it
	// as read: they sit past a hole, and ReadAt counts from off forward.
	w, c := resident(t, data, 8, 4)
	c.fail = errRead
	b := make([]byte, 8)
	if n, err := w.ReadAt(b, 0); !errors.Is(err, errRead) || n != 0 {
		t.Errorf("ReadAt over a failing head=%d %v, want 0 %v", n, err, errRead)
	}
	unmoved(t, w, data[4:12], 4, nil)

	// A failure reading the tail still counts the head and resident bytes.
	w, c = resident(t, data, 8, 4)
	c.fail = errRead
	if n, err := w.ReadAt(b, 8); !errors.Is(err, errRead) || n != 4 {
		t.Errorf("ReadAt over a failing tail=%d %v, want 4 %v", n, err, errRead)
	} else if string(b[:n]) != "89ab" {
		t.Errorf("ReadAt over a failing tail=%q, want %q", b[:n], "89ab")
	}
	unmoved(t, w, data[4:12], 4, nil)
}

func TestWindowReadAtUninitialized(t *testing.T) {
	var zero lexorg.Window
	if n, err := zero.ReadAt(make([]byte, 4), 0); !errors.Is(err, lexorg.ErrUninitialized) || n != 0 {
		t.Errorf("zero Window ReadAt=%d %v, want 0 %v", n, err, lexorg.ErrUninitialized)
	}

	// No fill buffer is no obstacle: ReadAt never needs one.
	var w lexorg.Window
	c := newCounter("abcd")
	w.Reset(c, nil)
	b := make([]byte, 4)
	if n, err := w.ReadAt(b, 0); err != nil || n != 4 || string(b) != "abcd" {
		t.Errorf("bufferless ReadAt=%d %q %v, want 4 %q nil", n, b[:n], err, "abcd")
	}
}

func TestWindowReadAtEdges(t *testing.T) {
	const data = "0123456789abcdef"
	w, _ := resident(t, data, 8, 4)

	// A negative offset is the reader's to reject, and it must not be mistaken
	// for a span overlapping the window.
	if n, err := w.ReadAt(make([]byte, 4), -1); err == nil || n != 0 {
		t.Errorf("ReadAt(-1)=%d %v, want 0 and an error", n, err)
	}
	unmoved(t, w, data[4:12], 4, nil)

	// An empty request reads nothing, whether or not it aliases the window.
	// Past the end of the file the underlying reader has its own say, which
	// ReadAt passes through rather than second-guessing.
	for _, off := range []int64{0, 6} {
		if n, err := w.ReadAt(nil, off); n != 0 || err != nil {
			t.Errorf("ReadAt(nil, %d)=%d %v, want 0 nil", off, n, err)
		}
	}
	if n, _ := w.ReadAt(nil, 20); n != 0 {
		t.Errorf("ReadAt(nil, 20)=%d bytes, want 0", n)
	}
}

func TestWindowReaderRead(t *testing.T) {
	const data = "0123456789abcdef"
	var wr lexorg.WindowReader
	wr.Reset(newCounter(data), make([]byte, 8), 0)

	// Read walks forward, and the cursor tracks what it consumed.
	var got []byte
	for {
		b := make([]byte, 5)
		n, err := wr.Read(b)
		got = append(got, b[:n]...)
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Read err=%v", err)
		}
		if want := int64(len(got)); wr.Offset() != want {
			t.Fatalf("Offset()=%d after %d bytes read", wr.Offset(), want)
		}
	}
	if string(got) != data {
		t.Errorf("Read walked %q, want %q", got, data)
	}
}

func TestWindowReaderReadByte(t *testing.T) {
	const data = "0123456789abcdef"
	// A buffer far smaller than the file so the cursor crosses the window
	// boundary repeatedly: that is where a byte used to repeat.
	c := newCounter(data)
	var wr lexorg.WindowReader
	wr.Reset(c, make([]byte, 4), 0)

	var got []byte
	for {
		b, err := wr.ReadByte()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("ReadByte err=%v", err)
		}
		got = append(got, b)
		if want := int64(len(got)); wr.Offset() != want {
			t.Fatalf("Offset()=%d after %d bytes read", wr.Offset(), want)
		}
	}
	if string(got) != data {
		t.Errorf("ReadByte walked %q, want %q", got, data)
	}
	// 16 bytes through a 4 byte window: four fills, then one that finds the end.
	if c.reads != 5 {
		t.Errorf("walking the file cost %d reads, want 5", c.reads)
	}
}

func TestWindowReaderReadRune(t *testing.T) {
	// Runes of every length, placed so sequences straddle a window boundary.
	const data = "héllo wörld — ¡añejo! 日本語"
	for _, bufsize := range []int{4, 5, 7, 8, 64} {
		var wr lexorg.WindowReader
		wr.Reset(newCounter(data), make([]byte, bufsize), 0)

		var got []rune
		for {
			r, k, err := wr.ReadRune()
			if err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("buf %d: ReadRune err=%v", bufsize, err)
			} else if r == utf8.RuneError && k <= 1 {
				t.Fatalf("buf %d: ReadRune failed to decode at %d", bufsize, wr.Offset())
			}
			got = append(got, r)
		}
		if string(got) != data {
			t.Errorf("buf %d: ReadRune walked %q, want %q", bufsize, string(got), data)
		}
	}
}

func TestWindowReaderResetReuse(t *testing.T) {
	const data = "0123456789abcdef"
	c := newCounter(data)
	buf := make([]byte, len(data)/2)
	var wr lexorg.WindowReader
	// A window starting inside the file, so an offset can fall on either side.
	start := int64(len(buf)) / 2
	wr.Reset(c, buf, start)

	if b, err := wr.ReadByte(); err != nil || b != data[start] {
		t.Fatalf("ReadByte at %d = %q %v, want %q", start, b, err, data[start])
	}
	if c.reads != 1 {
		t.Fatalf("setup cost %d reads, want 1", c.reads)
	}
	resident, base := wr.Buffer()
	end := base + int64(len(resident))

	// Reset to the same reader and buffer at an offset inside the resident
	// bytes moves the cursor and nothing else.
	for _, off := range []int64{base, (base + end) / 2, end - 1} {
		wr.Reset(c, buf, off)
		if wr.Offset() != off {
			t.Errorf("Reset to %d left Offset()=%d", off, wr.Offset())
		}
		if b, err := wr.ReadByte(); err != nil || b != data[off] {
			t.Errorf("Reset to %d then ReadByte=%q %v, want %q", off, b, err, data[off])
		}
	}
	if c.reads != 1 {
		t.Errorf("resetting inside the window cost %d reads, want 0", c.reads-1)
	}

	// The offset just past the resident bytes is where a forward scan stops, so
	// it keeps them: the cursor sits at the edge rather than outside.
	wr.Reset(c, buf, end)
	if wr.Offset() != end {
		t.Errorf("Reset to the window's edge left Offset()=%d, want %d", wr.Offset(), end)
	}
	if got, gotBase := wr.Buffer(); gotBase != base || len(got) != len(resident) {
		t.Errorf("Reset to the window's edge dropped the bytes: %d@%d, want %d@%d",
			len(got), gotBase, len(resident), base)
	}

	// An offset past that still costs no read at Reset time, and the fill
	// buffer survives: only the first read pays.
	wr.Reset(c, buf, end+1)
	if wr.Offset() != end+1 {
		t.Errorf("Reset outside the window left Offset()=%d, want %d", wr.Offset(), end+1)
	}
	if c.reads != 1 {
		t.Errorf("Reset outside the window cost %d reads, want 0", c.reads-1)
	}
	if b, err := wr.ReadByte(); err != nil || b != data[end+1] {
		t.Errorf("ReadByte after reset outside=%q %v, want %q", b, err, data[end+1])
	}
	if c.reads != 2 {
		t.Errorf("first read after reset cost %d reads, want 1", c.reads-1)
	}

	// A different reader drops the bytes but keeps the buffer.
	const other = "FEDCBA9876543210"
	wr.Reset(newCounter(other), nil, start)
	if wr.Offset() != start {
		t.Errorf("Reset to another reader left Offset()=%d, want %d", wr.Offset(), start)
	}
	if b, err := wr.ReadByte(); err != nil || b != other[start] {
		t.Errorf("ReadByte on the new reader=%q %v, want %q", b, err, other[start])
	}
}

func TestWindowReaderDrop(t *testing.T) {
	const data = "0123456789abcdef"
	c := newCounter(data)
	var wr lexorg.WindowReader
	wr.Reset(c, make([]byte, 8), 0)

	for range 5 {
		if _, err := wr.ReadByte(); err != nil {
			t.Fatalf("ReadByte err=%v", err)
		}
	}
	if wr.Offset() != 5 {
		t.Fatalf("Offset()=%d after 5 bytes, want 5", wr.Offset())
	}

	// Drop distrusts the bytes; it must not move the cursor.
	wr.Drop()
	if wr.Offset() != 5 {
		t.Errorf("Drop moved the cursor to %d, want 5", wr.Offset())
	}
	reads := c.reads
	if b, err := wr.ReadByte(); err != nil || b != '5' {
		t.Errorf("ReadByte after Drop=%q %v, want '5'", b, err)
	}
	if c.reads != reads+1 {
		t.Errorf("Drop then ReadByte cost %d reads, want 1", c.reads-reads)
	}
}

func TestWindowReaderMixed(t *testing.T) {
	// Read, ReadByte and ReadRune share one cursor, so interleaving them must
	// tile the input exactly.
	const data = "ab¡cdéfghij"
	var wr lexorg.WindowReader
	wr.Reset(newCounter(data), make([]byte, 6), 0)

	b, err := wr.ReadByte()
	if err != nil || b != 'a' {
		t.Fatalf("ReadByte=%q %v, want 'a'", b, err)
	}
	r, k, err := wr.ReadRune()
	if err != nil || r != 'b' || k != 1 {
		t.Fatalf("ReadRune=%q %d %v, want 'b' 1", r, k, err)
	}
	r, k, err = wr.ReadRune()
	if err != nil || r != '¡' || k != 2 {
		t.Fatalf("ReadRune=%q %d %v, want '¡' 2", r, k, err)
	}
	if wr.Offset() != 4 {
		t.Fatalf("Offset()=%d, want 4", wr.Offset())
	}
	rest := make([]byte, len(data)-4)
	if n, err := io.ReadFull(&wr, rest); err != nil {
		t.Fatalf("ReadFull=%d %v", n, err)
	}
	if string(rest) != data[4:] {
		t.Errorf("ReadFull=%q, want %q", rest, data[4:])
	}
	if wr.Offset() != int64(len(data)) {
		t.Errorf("Offset()=%d at end, want %d", wr.Offset(), len(data))
	}
}

func TestWindowReaderBuffer(t *testing.T) {
	// The resident bytes are what a caller aliases to hand back a span it has
	// already read, so the cursor's window must be visible through the reader.
	const data = "0123456789abcdef"
	var wr lexorg.WindowReader
	wr.Reset(newCounter(data), make([]byte, 6), 4)

	// Reset parks base at the requested offset over an empty window.
	if buf, base := wr.Buffer(); len(buf) != 0 || base != 4 {
		t.Errorf("Buffer() before any read=%q @%d, want empty @4", buf, base)
	}
	for range 3 {
		if _, err := wr.ReadByte(); err != nil {
			t.Fatalf("ReadByte err=%v", err)
		}
	}
	buf, base := wr.Buffer()
	if base != 4 {
		t.Fatalf("Buffer() base=%d, want 4", base)
	}
	if string(buf) != data[base:base+int64(len(buf))] {
		t.Errorf("Buffer()=%q, want %q", buf, data[base:base+int64(len(buf))])
	}
	// The cursor stands inside what it reports, which is what makes an alias of
	// a span already passed valid.
	if off := wr.Offset(); off < base || off > base+int64(len(buf)) {
		t.Errorf("Offset()=%d outside resident [%d,%d)", off, base, base+int64(len(buf)))
	}
}

func TestWindowReaderUninitialized(t *testing.T) {
	var wr lexorg.WindowReader
	if _, err := wr.ReadByte(); !errors.Is(err, lexorg.ErrUninitialized) {
		t.Errorf("zero WindowReader ReadByte err=%v, want %v", err, lexorg.ErrUninitialized)
	}
	if _, _, err := wr.ReadRune(); !errors.Is(err, lexorg.ErrUninitialized) {
		t.Errorf("zero WindowReader ReadRune err=%v, want %v", err, lexorg.ErrUninitialized)
	}
	if _, err := wr.Read(make([]byte, 4)); !errors.Is(err, lexorg.ErrUninitialized) {
		t.Errorf("zero WindowReader Read err=%v, want %v", err, lexorg.ErrUninitialized)
	}
}
