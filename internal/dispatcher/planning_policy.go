package dispatcher

import "github.com/nn1a/autogora/internal/boards"

// autoDecomposeEnabled is the single policy gate shared by the planning queue
// and coordination observer. Explicit command options retain their existing
// precedence over board and Autopilot settings.
func autoDecomposeEnabled(metadata boards.Metadata, options Options) bool {
	enabled := metadata.Orchestration.AutoDecompose
	if options.Autopilot {
		autopilot := metadata.Orchestration.Autopilot
		enabled = enabled && autopilot.Enabled && autopilot.AutoPlan
	}
	if options.AutoDecompose != nil {
		enabled = *options.AutoDecompose
	}
	return enabled
}
