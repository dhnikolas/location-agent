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
	if !info.IsDir() {
		// Docker can bind a single file, but a box is given directories to work
		// in. Allowing files would mean explaining that replacing one from an
		// editor breaks the mount, which is not worth the feature.
		return fmt.Errorf("hostPath %q is not a directory", hostPath)
	}
	return nil
}
