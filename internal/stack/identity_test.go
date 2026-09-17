package stack

import "testing"

func TestContainerIdentity(t *testing.T) {
	for _, test := range []struct {
		name, input, uid, gid string
		wantError             bool
	}{
		{"rootful", `["name=seccomp,profile=builtin","name=cgroupns"]`, "2001", "2002", false},
		{"rootless", `["name=seccomp,profile=builtin","name=rootless"]`, "0", "0", false},
		{"not rootless", `["name=not-rootless"]`, "2001", "2002", false},
		{"unmapped", `[]`, "2001", "2002", false},
		{"unsupported remap", `["name=userns"]`, "", "", true},
		{"malformed", `{}`, "", "", true},
		{"null", `null`, "", "", true},
		{"wrong type", `[false]`, "", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			uid, gid, err := containerIdentity([]byte(test.input), 2001, 2002)
			if (err != nil) != test.wantError || uid != test.uid || gid != test.gid {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, error=%v)", uid, gid, err, test.uid, test.gid, test.wantError)
			}
		})
	}
}
