package lexorg

import (
	"io"
	"unicode/utf8"
	"unsafe"
)

// Window is a byte window over an io.ReaderAt: buf holds the file bytes
// [base, base+len(buf)). Unlike a bufio.Reader it knows which file range it
// holds, so seeking to an offset already inside the Window costs no read at
// all. That suits lexing that jumps around a file — to an offset named by an
// index, past a payload, back to a span already scanned — since such jumps
// overwhelmingly land within a few hundred bytes of each other.
//
// The zero Window has no fill buffer and no reader; it becomes usable after a
// [Window.Reset] supplying both.
type Window struct {
	base int64  // File offset of buf[0].
	buf  []byte // Resident bytes; capacity is the fill size. Caller-owned when set through Reset.
	r    io.ReaderAt
	err  error // Last fill's error, io.EOF included, which Err does not report.
}

// Reset binds w to r, optionally replacing the fill buffer. len(buf) fixes the
// fill size; the bytes are read over, never read from. A nil buf keeps the
// current buffer, so a Window that no Reset has yet handed one to stays
// unusable: every [Window.ByteAt] fails with [lexorg.ErrUninitialized].
// Resetting to the reader already bound keeps the resident bytes, which is what
// makes repeated jumps around one file cost no reads.
//
// r's dynamic type must be comparable — every stdlib io.ReaderAt is, as is
// any pointer — because Reset tests r against the bound reader to decide
// whether the resident bytes still describe the same file.
func (w *Window) Reset(r io.ReaderAt, buf []byte) {
	w.err = nil
	// w.buf's capacity is the fill size, so it stands in for the length of the
	// buffer the caller handed over.
	newbuf := buf != nil &&
		(len(buf) != cap(w.buf) || unsafe.SliceData(buf) != unsafe.SliceData(w.buf))
	if newbuf {
		// A different buffer: base and the resident bytes describe the old one.
		// Handing back the same slice must stay a no-op — callers pass it on
		// every Reset, and dropping the bytes each time would defeat the point.
		w.buf = buf[:0:len(buf)]
	} else if w.r == r {
		return // Same reader and buffer: resident bytes still apply.
	}
	w.r = r
	w.buf = w.buf[:0]
}

// Err returns the read error, or nil if the file merely ended — [Window.EOF]
// reports that. It describes the last fill rather than latching the first
// failure, so a fill that succeeds clears it.
func (w *Window) Err() error { return w.err }

func (w *Window) Buffer() ([]byte, int64) { return w.buf, w.base }
func (w *Window) BufferCapacity() int     { return cap(w.buf) }
func (w *Window) BufferUsed() int         { return len(w.buf) }

// EOF reports whether the last fill ran off the end of the file.
func (w *Window) EOF() bool { return w.err == io.EOF }

// Drop discards the resident bytes and any recorded error, so the next ByteAt
// reads; the fill buffer survives. It is for a reader whose bytes at a given
// offset changed under the window — a reused cursor repointed at different
// underlying data — which the window cannot notice on its own: it keys what it
// holds by reader identity, and that identity did not change.
func (w *Window) Drop() {
	w.buf = w.buf[:0]
	w.err = nil
}

// ByteAt returns the file byte at off, refilling only when off falls outside
// the resident window.
func (w *Window) ByteAt(off int64) (byte, bool) {
	if i := off - w.base; i >= 0 && i < int64(len(w.buf)) {
		return w.buf[i], true
	}
	return w.Fill(off)
}

// Fill is ByteAt's slow path: off lies outside the resident bytes, so refill
// starting at off. Filling forward from the requested byte (rather than
// centering on it) keeps a forward scan to one read per cap(buf) bytes.
func (w *Window) Fill(off int64) (byte, bool) {
	if w.r == nil || cap(w.buf) == 0 {
		w.err = ErrUninitialized
		return 0, false
	} else if off < 0 {
		return 0, false
	} else if w.err != nil && off >= w.base {
		// The last fill ended the file or failed at base and off is at or past
		// it, so no read can turn up a byte. This is the end-of-file offset a
		// separate field would record: the resident bytes already cover
		// [base, off), leaving nothing between base and off to reread.
		return 0, false
	}
	n, err := w.r.ReadAt(w.buf[:cap(w.buf)], off)
	w.err = err
	if n == 0 && off <= w.base+int64(len(w.buf)) {
		// If fill finds nothing then keeping window costs nothing, keep it and store error if any.
		return 0, false
	}
	w.base, w.buf = off, w.buf[:n]
	if n == 0 {
		return 0, false
	}
	return w.buf[0], true
}

// View returns the n bytes at off as a subslice of the resident bytes, refilling
// only when the span is not already held whole. n may not exceed the fill size.
func (w *Window) View(off int64, n int) ([]byte, error) {
	if i := off - w.base; i >= 0 && n >= 0 && i+int64(n) <= int64(len(w.buf)) {
		return w.buf[i : i+int64(n) : i+int64(n)], nil
	} else if n < 0 || n > cap(w.buf) {
		return nil, ErrViewTooLarge
	}
	if _, ok := w.Fill(off); !ok {
		if w.err != nil {
			return nil, w.err
		}
		return nil, io.ErrUnexpectedEOF
	}
	if len(w.buf) < n {
		return w.buf[:len(w.buf):len(w.buf)], io.ErrUnexpectedEOF
	}
	return w.buf[:n:n], nil
}

// ReadAt copies the part of b the resident window already holds and reads the
// rest straight through to the underlying reader, honouring the io.ReaderAt
// contract: a short read always comes with a non-nil error. It leaves the
// window untouched — neither the buffer nor the recorded error move — so a
// bulk read of a span far from the cursor does not evict the bytes the cursor
// is working over.
func (w *Window) ReadAt(b []byte, off int64) (int, error) {
	if w.r == nil {
		return 0, ErrUninitialized
	} else if off < 0 {
		return w.r.ReadAt(b, off) // The reader names its own negative-offset error.
	}
	w0, w1 := w.base, w.base+int64(len(w.buf))
	b0, b1 := off, off+int64(len(b))
	lo, hi := max(w0, b0), min(w1, b1) // The span b and the window share.
	if lo >= hi {
		return w.r.ReadAt(b, off)
	}
	n := 0
	if b0 < lo {
		// b starts before the window: read up to it, and stop on a short read
		// rather than counting the resident bytes past the hole as read.
		nhead, err := w.r.ReadAt(b[:lo-b0], b0)
		n += nhead
		if err != nil {
			return n, err
		}
	}
	n += copy(b[lo-b0:hi-b0], w.buf[lo-w0:hi-w0])
	if hi < b1 {
		// b runs past the window. Same reasoning as above: if that fill hit the
		// end of the file, everything past it is gone and no read finds more.
		if w.err == io.EOF && hi == w1 {
			return n, io.EOF
		}
		ntail, err := w.r.ReadAt(b[hi-b0:], hi)
		return n + ntail, err
	}
	return n, nil
}

// ReaderAt returns the underlying ReaderAt implementation.
func (w *Window) ReaderAt() io.ReaderAt { return w.r }

// WindowReader is a sequential cursor over a [Window]: it reads forward from an
// offset the caller picks, refilling as it crosses the end of the resident
// bytes. The cursor is base+bufoff rather than an offset of its own, so the
// bytes a fill leaves resident are the bytes the cursor is standing on — a
// rewind inside them costs no read.
type WindowReader struct {
	w      Window
	bufoff int // Cursor's distance forward from the window's base.
}

// Offset returns the absolute file offset the next read starts at.
func (wr *WindowReader) Offset() int64 { return wr.w.base + int64(wr.bufoff) }

// Err returns the underlying window's error. See [Window.Err].
func (wr *WindowReader) Err() error { return wr.w.Err() }

// Read reads into b from the cursor and advances it by what it got.
func (wr *WindowReader) Read(b []byte) (n int, err error) {
	// ReadAt leaves the window alone, so the cursor is all that moves. bufoff
	// may end up past the resident bytes; the next fill rebases onto it.
	n, err = wr.w.ReadAt(b, wr.Offset())
	wr.bufoff += n
	return n, err
}

// ReadByte returns the byte at the cursor and advances it by one.
func (wr *WindowReader) ReadByte() (byte, error) {
	if wr.bufoff >= len(wr.w.buf) {
		b, ok := wr.Fill(wr.Offset())
		if !ok {
			return 0, wr.fillErr()
		}
		wr.bufoff = 1 // Fill zeroed it, and buf[0] is the byte being returned.
		return b, nil
	}
	b := wr.w.buf[wr.bufoff]
	wr.bufoff++
	return b, nil
}

// ReadRune decodes the UTF-8 sequence at the cursor and advances it past the
// bytes consumed. A sequence the file truncates decodes as [utf8.RuneError]
// over one byte, the same as decoding a malformed one. The fill buffer must be
// at least [utf8.UTFMax] bytes, or a sequence longer than it can never be held
// whole and decodes as RuneError however the cursor is placed.
func (wr *WindowReader) ReadRune() (rune, int, error) {
	const utf8lead = 0x80 // Set on every byte of a multi-byte sequence.
	avail := len(wr.w.buf) - wr.bufoff
	// A sequence running past the resident bytes would decode as RuneError, so
	// refill first when the cursor leads one the window may not hold whole. A
	// window that already reaches the end of the file holds all there is to
	// hold, and refilling it would only rediscover that end.
	straddles := avail > 0 && !wr.w.EOF() &&
		wr.w.buf[wr.bufoff]&utf8lead != 0 && avail < utf8.UTFMax
	if avail <= 0 || straddles {
		if _, ok := wr.Fill(wr.Offset()); !ok {
			return utf8.RuneError, 0, wr.fillErr()
		}
	}
	r, k := utf8.DecodeRune(wr.w.buf[wr.bufoff:])
	wr.bufoff += k
	return r, k, nil
}

// fillErr reports why a fill turned up nothing. A window that simply found no
// bytes records no error, which for a reader is the end of the input.
func (wr *WindowReader) fillErr() error {
	if err := wr.w.Err(); err != nil {
		return err
	}
	return io.EOF
}

// Fill refills the window from off and puts the cursor there.
func (wr *WindowReader) Fill(off int64) (byte, bool) {
	wr.bufoff = 0
	return wr.w.Fill(off)
}

// Reset binds wr to r at absolute offset, optionally replacing the fill buffer;
// [Window.Reset] describes how buf is handled. Resetting to the reader and
// buffer already bound keeps the resident bytes, so an offset landing inside
// them costs no read at all — only the cursor moves.
func (wr *WindowReader) Reset(r io.ReaderAt, buf []byte, offset int64) {
	wr.w.Reset(r, buf)
	if i := offset - wr.w.base; i >= 0 && i <= int64(len(wr.w.buf)) {
		// Check above uses <= since if it lands at edge of existing buffer
		// the user may still want to read previous data.
		wr.bufoff = int(i) // offset is resident: reaching it needs no read.
		return
	}
	// Nothing resident covers offset, so park the cursor there over an empty
	// window and let the first read fill from it. The fill buffer survives, so
	// this still costs no read and no allocation.
	wr.w.base, wr.w.buf = offset, wr.w.buf[:0]
	wr.bufoff = 0
}

// Drop discards the resident bytes without moving the cursor. See [Window.Drop].
func (wr *WindowReader) Drop() {
	// Rebasing onto the cursor keeps Offset put: Drop distrusts the bytes, it
	// does not seek.
	wr.w.base, wr.bufoff = wr.Offset(), 0
	wr.w.Drop()
}

// Buffer returns the resident bytes and the file offset of first byte buf[0].
func (wr *WindowReader) Buffer() ([]byte, int64) { return wr.w.Buffer() }

// ReaderAt returns the underlying ReaderAt.
func (wr *WindowReader) ReaderAt() io.ReaderAt {
	return wr.w.ReaderAt()
}

// ReadView returns the next n bytes as a subslice of the resident bytes and advances
// the cursor past them. Can only read window-sized amount of bytes. See [Window.View].
func (wr *WindowReader) ReadView(n int) ([]byte, error) {
	off := wr.Offset()
	b, err := wr.w.View(off, n)
	if err != nil {
		return b, err
	}
	// View may have refilled and rebased the window, so recompute the cursor
	// from the absolute offset rather than trusting the old bufoff.
	wr.bufoff = int(off-wr.w.base) + len(b)
	return b, nil
}
