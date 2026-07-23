package skills

import "embed"

// Names lists the portable skills shipped with every Autogora binary.
var Names = []string{"autogora-worker", "autogora-coordinator"}

// Files contains the complete portable skill directories.
//
//go:embed autogora-worker autogora-coordinator
var Files embed.FS
