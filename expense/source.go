package expense

import _ "embed"

// WorkflowSource is this package's workflow.go, served to the demo's code
// panel so the excerpt on screen is always the code that is running.
//
//go:embed workflow.go
var WorkflowSource string
