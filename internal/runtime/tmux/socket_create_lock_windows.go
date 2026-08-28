//go:build windows

package tmux

// lockSocketCreateFile is a no-op on Windows: tmux does not run there, so the
// named-socket creation race this lock guards cannot occur. The in-process
// creation lock still applies.
func lockSocketCreateFile(string) (func(), error) {
	return func() {}, nil
}
