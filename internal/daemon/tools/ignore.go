package tools

// ignoredDirs are skipped by list_dir and grep — noise directories that
// are almost never what an agent is looking for and can be huge
// (node_modules, vendor caches, build output).
var ignoredDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".turbo":       true,
	".next":        true,
	"target":       true,
	// .kram holds the daemon's own live SQLite database and logs —
	// scanning it as text was a real, observed bug: grep walked into a
	// binary database file mid-write and returned garbage control-byte
	// matches, which is exactly the kind of confusing tool result that
	// can derail a weak model's next turn.
	".kram": true,
}

func isIgnoredName(name string) bool {
	return ignoredDirs[name]
}
