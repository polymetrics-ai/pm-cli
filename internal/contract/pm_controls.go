package contract

// IsReservedPMControlFlag reports whether flag is an exact PM-owned long
// option. Provider command fields must be aliased before reaching a client
// descriptor when their projected flag appears in this registry.
func IsReservedPMControlFlag(flag string) bool {
	switch flag {
	case "--connection",
		"--help",
		"--json",
		"--lane",
		"--output",
		"--request-key",
		"--request-timeout",
		"--result-controls",
		"--server-profile",
		"--session-store",
		"--wait",
		"--workspace":
		return true
	default:
		return false
	}
}
