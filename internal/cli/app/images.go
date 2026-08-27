package app

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// imageCommandPrefix is the composer command that stages an image path for
// the next message. The trailing space is part of the token, so a bare
// "/image" (no path) falls through to being sent as an ordinary message.
const imageCommandPrefix = "/image "

// maxImageBytes caps a single attached image. Images ride in every request
// for the rest of the session (they're history), so a runaway file would
// bloat both memory and the token budget; 20 MiB is comfortably above any
// real screenshot while still bounding the worst case.
const maxImageBytes = 20 << 20

// stageImageForAttachment validates a /image path (exists, is a regular
// file, within the size cap, and actually contains image data) and returns
// the cleaned path to stage. The content check reads only the leading bytes,
// so it's cheap enough to run on the keystroke — and it means a mistaken
// /image on a text file (even one renamed with a .png extension) fails
// immediately with a clear message instead of at send time. Full decoding
// is still deferred to send time (imageFileToDataURL).
func stageImageForAttachment(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("usage: /image <path>")
	}
	path = expandUser(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not an image", filepath.Base(path))
	}
	if info.Size() > maxImageBytes {
		return "", fmt.Errorf("%s is %d MB, larger than the %d MB limit", filepath.Base(path), info.Size()>>20, int64(maxImageBytes)>>20)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	if mime := http.DetectContentType(head[:n]); !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("%s is not an image (looks like %s)", filepath.Base(path), mime)
	}
	return path, nil
}

// imageFileToDataURL reads an image file and returns it as a base64 data
// URL (data:<mime>;base64,<...>) — the shape store.Message.Images holds and
// the provider adapters decode (see internal/provider/*.go parseDataURL).
// The MIME type is sniffed from the actual bytes, not the extension, so a
// non-image file (even one named *.png) is rejected loudly rather than
// shipped to the model as garbage. Defense in depth: stageImageForAttachment
// already ran the same check, but the file could have changed since /image.
func imageFileToDataURL(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if info.Size() > maxImageBytes {
		return "", fmt.Errorf("%s is larger than the %d MB limit", filepath.Base(path), int64(maxImageBytes)>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("%s is not an image (looks like %s)", filepath.Base(path), mime)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// expandUser expands a leading ~ to the user's home directory, so
// /image ~/shot.png works. Any failure leaves the path untouched.
func expandUser(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// imageDisplayNames turns staged file paths into base names for the
// transcript/composer, deduping nothing — the user attached them in order.
func imageDisplayNames(paths []string) []string {
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}
	return names
}
