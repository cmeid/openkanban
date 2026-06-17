package config

// SetGuardedDirsForTest swaps the captured guarded dirs for a test and
// returns a restore func. Test-only (export_test.go compiles only into the
// config package test binary).
func SetGuardedDirsForTest(dirs []string) (restore func()) {
	prev := guardedRealDirs
	guardedRealDirs = dirs
	return func() { guardedRealDirs = prev }
}
