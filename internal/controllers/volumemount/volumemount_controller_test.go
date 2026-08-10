package volumemount

import "testing"

// What docker reports a bind's source to be is not always what it was given,
// and the difference is invisible from inside the container — the mount works
// either way. It shows up only here, as a mount that never stops saying it is
// not mounted yet.
func TestSameHostPath(t *testing.T) {
	cases := []struct {
		name     string
		reported string
		want     string
		equal    bool
	}{
		{"linux, engine and machine are one host", "/Users/me/code", "/Users/me/code", true},
		{"docker desktop shares the filesystem", "/host_mnt/Users/me/code", "/Users/me/code", true},
		{"docker desktop proxies its own socket", "/run/host-services/docker.proxy.sock", "/var/run/docker.sock", true},
		{"the same socket under its other name", "/run/host-services/docker.proxy.sock", "/run/docker.sock", true},

		{"a different directory", "/host_mnt/Users/me/other", "/Users/me/code", false},
		{"nothing reported", "", "/Users/me/code", false},
		// The socket substitution is believable only for the socket Docker
		// Desktop proxies. Anything else reported under that name is a mount
		// that is not what was asked for, and saying it is ready would be a lie
		// nobody could see through from the container.
		{"the proxy socket standing in for something else", "/run/host-services/docker.proxy.sock", "/var/run/other.sock", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameHostPath(tc.reported, tc.want); got != tc.equal {
				t.Errorf("sameHostPath(%q, %q) = %v, want %v", tc.reported, tc.want, got, tc.equal)
			}
		})
	}
}
