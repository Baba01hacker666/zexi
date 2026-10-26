package lsp

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// jsonrpc read/write with LSP Content-Length framing.

// readMessage reads one framed JSON-RPC message. It returns io.EOF when the
// stream ends cleanly between messages.
func readMessage(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && strings.TrimSpace(line) == "" {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header: %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("bad Content-Length: %q", value)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// writeMessage writes one framed JSON-RPC message.
func writeMessage(w io.Writer, body []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}
