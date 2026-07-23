package skills

import "embed"

// Names lists the portable skills shipped with every Autogora binary.
var Names = []string{"autogora-worker", "autogora-orchestrator"}

// Files contains the complete portable skill directories.
//
//go:embed autogora-worker autogora-orchestrator
var Files embed.FS
