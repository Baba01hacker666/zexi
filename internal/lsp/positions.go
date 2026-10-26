package lsp

// Position conversions between LSP coordinates (0-based line, 0-based UTF-16
// code-unit column) and byte offsets in a file's source. Zexi counts columns
// in bytes, so both directions must walk the text.

// offsetToPosition converts a byte offset into an LSP position.
func offsetToPosition(src []byte, offset int) Position {
	if offset < 0 {
		offset = 0
	}
	if offset > len(src) {
		offset = len(src)
	}
	line, lineStart := 0, 0
	for i, b := range src {
		if i >= offset {
			break
		}
		if b == '\n' {
			line++
			lineStart = i + 1
		}
	}
	return Position{Line: line, Character: utf16Len(src[lineStart:offset])}
}

// positionToOffset converts an LSP position into a byte offset, clamping to
// the source. Out-of-range lines land at the end of the file; out-of-range
// columns land at the end of the line.
func positionToOffset(src []byte, pos Position) int {
	line, lineStart := 0, 0
	for i, b := range src {
		if line == pos.Line {
			break
		}
		if b == '\n' {
			line++
			lineStart = i + 1
		}
	}
	if line < pos.Line {
		return len(src)
	}
	// Walk the line up to the requested UTF-16 column.
	units, off := 0, lineStart
	for off < len(src) && src[off] != '\n' && units < pos.Character {
		w := runeWidth(src, off)
		// A non-BMP rune counts as two UTF-16 code units.
		if w == 4 {
			units += 2
		} else {
			units++
		}
		if units > pos.Character {
			// The requested column splits a surrogate pair; land before it.
			return off
		}
		off += w
	}
	return off
}

// utf16Len returns the number of UTF-16 code units in s.
func utf16Len(s []byte) int {
	units := 0
	for i := 0; i < len(s); {
		w := runeWidth(s, i)
		if w == 4 {
			units += 2
		} else {
			units++
		}
		i += w
	}
	return units
}

// runeWidth returns the byte width of the UTF-8 rune starting at s[i].
// Invalid bytes count as one.
func runeWidth(s []byte, i int) int {
	b := s[i]
	switch {
	case b < 0x80:
		return 1
	case b&0xE0 == 0xC0:
		return 2
	case b&0xF0 == 0xE0:
		return 3
	case b&0xF8 == 0xF0:
		return 4
	}
	return 1
}

// posInRange reports whether a byte offset lies in [start, end) of a span.
func posInRange(offset, start, end int) bool {
	return offset >= start && offset < end
}
