package runtime

import (
	"fmt"
	"os"
)

// CheckMount answers whether a host directory can be bound into a box right
// now: the shape rules from ValidateMount, plus the questions only this machine
// can answer.
//
// The directory has to exist already. Docker would happily create a missing one
// — owned by root, empty — and the result of a mistyped path would look exactly
// like a directory whose contents went missing. Refusing costs the user one
// `mkdir` and saves them that.
//
// What cannot be checked here is Docker Desktop's file sharing: a path outside
// the shared list mounts as an empty directory, with no error anywhere. The
// list is not readable from a supported interface, so a box that comes up with
// an empty mount despite this check is the sign to go and look at
// Settings → Resources → File sharing.
func CheckMount(hostPath, containerPath string) error {
	if err := ValidateMount(hostPath, containerPath); err != nil {
		return err
	}

	info, err := os.Stat(hostPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("hostPath %q does not exist on this machine", hostPath)
	}
	if err != nil {
		return fmt.Errorf("hostPath %q: %w", hostPath, err)
	}
	switch {
	case info.IsDir():
	case info.Mode()&os.ModeSocket != 0:
		// A socket is bound as itself and lives as long as whatever listens on
		// it — the machine's docker socket is the reason anyone asks for one.
		// Nothing rewrites a socket the way an editor rewrites a file, so the
		// objection below does not apply to it.
	default:
		// Docker can bind a single file, but a box is given directories to work
		// in. Allowing files would mean explaining that replacing one from an
		// editor breaks the mount — the editor writes a new file, the bind stays
		// on the old one, and the edits appear not to arrive.
		return fmt.Errorf("hostPath %q is neither a directory nor a socket", hostPath)
	}
	return nil
}
