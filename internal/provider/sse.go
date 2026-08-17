package provider

import (
	"bufio"
	"io"
	"strings"
)

// scanSSEData reads a Server-Sent Events stream and calls fn once per
// "data: ..." payload, stripped of the prefix. Lines that are not data
// events (comments, event:, id:, blank separators) are skipped. It stops
// on read error or when fn returns false.
func scanSSEData(r io.Reader, fn func(data string) (keepGoing bool)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if !fn(data) {
			return nil
		}
	}
	return scanner.Err()
}
