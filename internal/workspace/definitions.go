package workspace

// Tool names exposed to the model.
const (
	CronjobManageName = "cronjob_manage"
)

// Workspace tool contracts are composed in stable registration order by
// tools.BuiltinDefinitions; each Definition lives next to its handler.

func float64Ptr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }
