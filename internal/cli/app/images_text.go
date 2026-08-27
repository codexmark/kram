package app

// User-facing strings for image attachment (the /image composer command,
// staged-attachment indicator, and per-message attachment row). English —
// see issue #74.
const (
	// pendingImagesPrefix leads the staged-attachments line shown just above
	// the composer while one or more images are queued for the next message.
	pendingImagesPrefix = "📎 attaching (sends with your next message): "
	// messageImagePrefix leads the attachment row shown under a sent user
	// message that carried images.
	messageImagePrefix = "📎 "
)
