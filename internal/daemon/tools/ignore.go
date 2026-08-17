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
}

func isIgnoredName(name string) bool {
	return ignoredDirs[name]
}
