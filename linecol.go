package lexorg

import (
	"bytes"
	"errors"
	"io"
)

// ToLineCol converts a byte offset to 1-indexed line:column for diagnostics.
// Also returns the length of the line containing the offset. aux is a scratch
// buffer used for reading; its size determines read chunk size (1024B
// recommended). This only makes sense for plaintext/utf8 character-set files.
func ToLineCol(r io.ReaderAt, pos int64, aux []byte) (line, col, lineLength int, err error) {
	offset := int(pos)
	if r == nil || offset < 0 {
		return 0, 0, 0, errors.New("invalid reader or offset")
	}

	line = 1
	lastNewlinePos := -1 // byte position of last newline seen (-1 means before start of file)

	// Read source up to offset to count newlines
	for readPos := 0; readPos < offset; {
		toRead := min(len(aux), offset-readPos)
		n, rerr := r.ReadAt(aux[:toRead], int64(readPos))
		if n == 0 && rerr != nil {
			return 0, 0, 0, rerr
		}

		// Count newlines and find last newline position in this chunk
		chunk := aux[:n]
		for {
			idx := bytes.IndexByte(chunk, '\n')
			if idx < 0 {
				break
			}
			line++
			lastNewlinePos = readPos + (n - len(chunk)) + idx
			chunk = chunk[idx+1:]
		}

		readPos += n
		if rerr == io.EOF {
			break
		}
	}

	col = offset - lastNewlinePos

	// Find line length by reading until next newline or EOF
	lineLength = col - 1 // at minimum, the portion before offset
	for readPos := offset; ; {
		n, rerr := r.ReadAt(aux[:], int64(readPos))
		if n == 0 && rerr != nil {
			break
		}
		idx := bytes.IndexByte(aux[:n], '\n')
		if idx >= 0 {
			lineLength = (readPos - lastNewlinePos - 1) + idx
			break
		}
		readPos += n
		if rerr == io.EOF {
			lineLength = readPos - lastNewlinePos - 1
			break
		}
	}

	return line, col, lineLength, nil
}
