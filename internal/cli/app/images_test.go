package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngBytes is a valid PNG signature followed by filler — enough for both
// the extension map and http.DetectContentType's magic-byte sniffing.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStageImageForAttachment(t *testing.T) {
	png := writeTemp(t, "shot.png", pngBytes)
	if got, err := stageImageForAttachment(png); err != nil || got != png {
		t.Errorf("valid image: got %q, err %v", got, err)
	}
	// Blank path is a usage error, not a stage.
	if _, err := stageImageForAttachment("   "); err == nil {
		t.Error("blank path: expected a usage error")
	}
	// Missing file.
	if _, err := stageImageForAttachment(filepath.Join(t.TempDir(), "nope.png")); err == nil {
		t.Error("missing file: expected an error")
	}
	// A directory is not an image.
	if _, err := stageImageForAttachment(t.TempDir()); err == nil {
		t.Error("directory: expected an error")
	}
	// A text file wearing a .png extension is rejected NOW (content is
	// sniffed at stage time), not silently staged to fail later.
	mislabeled := writeTemp(t, "notreally.png", []byte("this is plainly text, not an image at all\n"))
	if _, err := stageImageForAttachment(mislabeled); err == nil {
		t.Error("text file named .png: expected a not-an-image error at stage time")
	}
}

func TestImageFileToDataURL(t *testing.T) {
	png := writeTemp(t, "shot.png", pngBytes)
	url, err := imageFileToDataURL(png)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("data URL = %q, want a data:image/png;base64, prefix", url[:min(40, len(url))])
	}

	// The guard is on the actual bytes, not the extension: a text file named
	// *.png is rejected (the defect the extension fast path used to hide),
	// and so is an extensionless text file.
	for _, name := range []string{"notreally.png", "notes"} {
		txt := writeTemp(t, name, []byte("just some notes, definitely not an image\n"))
		if _, err := imageFileToDataURL(txt); err == nil {
			t.Errorf("%s: expected a not-an-image error", name)
		}
	}
}

func TestImageDisplayNames(t *testing.T) {
	got := imageDisplayNames([]string{"/home/u/a.png", "rel/b.jpg"})
	if len(got) != 2 || got[0] != "a.png" || got[1] != "b.jpg" {
		t.Errorf("imageDisplayNames = %v, want [a.png b.jpg]", got)
	}
}

// TestImageAttachmentFlow drives the composer end to end: /image stages a
// file (without sending), a bad path errors without staging, and the next
// real message attaches the staged image and clears the queue.
func TestImageAttachmentFlow(t *testing.T) {
	m := testModel(t)
	png := writeTemp(t, "shot.png", pngBytes)

	// /image stages, does not send.
	m.input.SetValue(imageCommandPrefix + png)
	m, cmd := modelResult(m.submit())
	if cmd != nil {
		t.Error("/image should not start a send")
	}
	if m.waiting {
		t.Error("/image should stage, not send")
	}
	if len(m.pendingImages) != 1 || m.pendingImages[0] != png {
		t.Fatalf("staged images = %v, want [%s]", m.pendingImages, png)
	}

	// A bad path surfaces an error and does not add a second staged image.
	m.input.SetValue(imageCommandPrefix + filepath.Join(t.TempDir(), "missing.png"))
	m, _ = modelResult(m.submit())
	if len(m.pendingImages) != 1 {
		t.Errorf("a bad /image path must not stage, got %v", m.pendingImages)
	}
	if m.err == nil {
		t.Error("a bad /image path should surface an error")
	}

	// The next real message attaches the staged image and clears the queue.
	m.input.SetValue("what is in this screenshot")
	m, cmd = modelResult(m.submit())
	if cmd == nil {
		t.Error("sending a message should return a command")
	}
	if len(m.pendingImages) != 0 {
		t.Errorf("pendingImages should be cleared after send, got %v", m.pendingImages)
	}
	var last chatMessage
	for _, msg := range m.messages {
		if msg.Role == "user" {
			last = msg
		}
	}
	if len(last.Images) != 1 || last.Images[0] != "shot.png" {
		t.Errorf("sent user message Images = %v, want [shot.png]", last.Images)
	}
}

// TestImageOnlyMessageIsAllowed confirms a staged image with no typed text
// still sends (the message carries just the image).
func TestImageOnlyMessageIsAllowed(t *testing.T) {
	m := testModel(t)
	png := writeTemp(t, "shot.png", pngBytes)
	m.pendingImages = []string{png}
	m.input.SetValue("")
	m, cmd := modelResult(m.submit())
	if cmd == nil || !m.waiting {
		t.Error("an image-only message should send")
	}
	if len(m.pendingImages) != 0 {
		t.Error("pendingImages should be cleared")
	}
}
