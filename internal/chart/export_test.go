package chart

// CheckPinned exposes checkPinned to the external test package.
func CheckPinned(config, which string, list []byte) (string, error) {
	return checkPinned(config, which, list)
}
