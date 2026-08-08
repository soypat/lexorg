package lexorg

import "io"

// StreamReaderAt adapts a forward-only io.Reader to an io.ReaderAt by retaining
// the bytes it has read in two buffers filled alternately. Fills are sequential
// so the two spans abut, making them one contiguous retained range that a read
// may cross; flipping between them retains twice a buffer without ever copying
// bytes down to compact them.
//
// It departs from io.ReaderAt in the two ways a stream forces. Parallel ReadAt
// calls are not supported: a stream has one position and this is it. And an
// offset that has fallen behind the retained range reports [ErrRewound] rather
// than reading, because the bytes are gone and the stream cannot go back for
// them. A read ahead of the retained range is fine at any size — the stream is
// consumed forward until it is satisfied, so a request larger than both buffers
// still works, it simply cannot be reread afterwards.
//
// The zero StreamReaderAt is unusable; [StreamReaderAt.Reset] supplies the
// reader and the buffer.
type StreamReaderAt struct {
	r io.Reader
	// Yield is called when the reader hands back no bytes and no error, which
	// io.Reader discourages without forbidding. consecutiveYields counts such
	// reads in a row and starts at 1, resetting as soon as bytes arrive, so it
	// is what a backoff or an attempt limit keys off. Returning false gives up
	// on the stream and fails the fill with io.ErrNoProgress.
	//
	// It is where waiting policy lives: runtime.Gosched to let a producer run,
	// a sleep to stop spinning, a deadline check to bail out. A nil Yield gives
	// up on the first such read, rather than have this package pick a spin
	// count on the caller's behalf. Reset keeps it, since it configures s
	// rather than describing the stream bound to it.
	Yield func(consecutiveYields uint) bool
	bufs  [2][]byte // Retained bytes; length is what the fill wrote.
	base  [2]int64  // Stream offset of bufs[i][0]. The two spans abut.
	cur   uint8     // Buffer the last fill wrote; the other holds the older span.
	err   error     // Sticky: unlike a file, a stream cannot be retried.
}

// Reset binds s to r, reading through buf split into the two halves it fills
// alternately, and starts offsets over at zero. A nil buf keeps the current
// halves. Half of len(buf) bounds how far back a read may go, so it wants to be
// at least twice the fill size of any [Window] layered over s.
func (s *StreamReaderAt) Reset(r io.Reader, buf []byte) {
	if buf != nil {
		half := len(buf) / 2
		s.bufs[0] = buf[:0:half]
		s.bufs[1] = buf[half:half:len(buf)]
	} else {
		s.bufs[0] = s.bufs[0][:0]
		s.bufs[1] = s.bufs[1][:0]
	}
	// A new stream shares nothing with the old one, so nothing survives but the
	// buffers themselves.
	s.r, s.err, s.cur = r, nil, 0
	s.base = [2]int64{}
}

// Err returns the error that ended the stream, or nil while it still reads.
// io.EOF is included, since for a stream that is where the bytes stop.
func (s *StreamReaderAt) Err() error { return s.err }

// Retained returns the offset range ReadAt can serve without touching the
// stream. Its low end is what [ErrRewound] is measured against.
func (s *StreamReaderAt) Retained() (start, end int64) {
	return s.base[1-s.cur], s.base[s.cur] + int64(len(s.bufs[s.cur]))
}

// ReadAt implements io.ReaderAt over the stream, reading forward as needed.
func (s *StreamReaderAt) ReadAt(b []byte, off int64) (int, error) {
	if s.r == nil || cap(s.bufs[0]) == 0 || cap(s.bufs[1]) == 0 {
		return 0, ErrUninitialized
	}
	start, _ := s.Retained()
	if off < start {
		return 0, ErrRewound
	}
	n := 0
	for n < len(b) {
		at := off + int64(n)
		if _, end := s.Retained(); at >= end {
			// Nothing retained reaches the cursor, so consume the next span. A
			// far-ahead offset just fills until the stream catches up to it.
			if s.err != nil || !s.fill() {
				return n, s.readErr()
			}
			continue
		}
		n += s.copyFrom(b[n:], at)
	}
	return n, nil
}

// fill reads the next span into the older buffer and makes it the newer one,
// so what the last fill left resident becomes the older span.
func (s *StreamReaderAt) fill() bool {
	next := 1 - s.cur
	_, end := s.Retained()
	// Read the span whole rather than taking one Read's worth, so a span comes
	// up short only at the end of the stream: a partial fill would shrink the
	// retained range for no reason and flip the buffers sooner than needed.
	b := s.bufs[next][:cap(s.bufs[next])]
	var (
		n     int
		empty uint
		err   error
	)
	for n < len(b) {
		nn, rerr := s.r.Read(b[n:])
		n, err = n+nn, rerr
		if rerr != nil {
			break
		} else if nn > 0 {
			empty = 0
			continue
		}
		// The read made no progress and did not fail, so wait however Yield
		// says to and stop when it stops asking for more.
		if empty++; s.Yield == nil || !s.Yield(empty) {
			err = io.ErrNoProgress
			break
		}
	}
	s.err = err
	if n == 0 {
		return false // Nothing came back, so keep the older span rather than spend it.
	}
	s.base[next], s.bufs[next] = end, s.bufs[next][:n]
	s.cur = next
	return true
}

// copyFrom copies into dst the retained bytes from stream offset at onward,
// crossing out of the older buffer into the newer one when the two abut.
func (s *StreamReaderAt) copyFrom(dst []byte, at int64) int {
	n := 0
	for _, i := range [2]uint8{1 - s.cur, s.cur} { // Oldest span first.
		buf := s.bufs[i]
		if j := at + int64(n) - s.base[i]; j >= 0 && j < int64(len(buf)) {
			n += copy(dst[n:], buf[j:])
		}
	}
	return n
}

// readErr reports why a fill turned up nothing. A stream that simply ran out
// records io.EOF; the nil case is a reader that returned no bytes and no error.
func (s *StreamReaderAt) readErr() error {
	if s.err != nil {
		return s.err
	}
	return io.EOF
}
