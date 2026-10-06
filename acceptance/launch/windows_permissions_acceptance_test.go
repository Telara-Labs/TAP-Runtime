//go:build windows

package launch

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Inspect native DACLs rather than Go's Unix-style mode bits. This checks the
// actual owner and the broad well-known Windows groups; it does not claim a
// second-account logon or protection from administrators/System.
func windowsPrivateACL(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		t.Fatalf("native security descriptor for %s: %v, %v", path, sd, err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		t.Fatalf("native owner: %v, %v", owner, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Equals(user.User.Sid) {
		t.Fatalf("%s owner %s differs from runner user %s", path, owner.String(), user.User.Sid.String())
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("%s has no restricting native DACL: %v", path, err)
	}
	t.Logf("native ACL %s: %s", path, sd.String())
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("unhandled native ACL entry type %d; cannot establish privacy", ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		broad := sid.IsWellKnown(windows.WinWorldSid) || sid.IsWellKnown(windows.WinAuthenticatedUserSid) || sid.IsWellKnown(windows.WinBuiltinUsersSid)
		access := uint32(ace.Mask)
		if broad && access&(windows.GENERIC_ALL|windows.GENERIC_READ|windows.GENERIC_WRITE|windows.FILE_READ_DATA|windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA|windows.WRITE_DAC|windows.WRITE_OWNER) != 0 {
			t.Errorf("%s grants broad group %s content or permission access: %#x", path, sid.String(), access)
		}
	}
}

func TestPublishedWindowsOutputACL(t *testing.T) {
	v := version(t)
	m := newMachine(t)
	tap := npmTap(t, m, v)
	work := filepath.Join(m.home, "owned output café")
	primitive := filepath.Join(work, "acl-output")
	if err := os.MkdirAll(primitive, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.example, name: acl-output, version: 0.1.0}\nexecution: {entrypoint: main.py}\nfiles:\n  - {path: out, access: write}\n"
	for name, body := range map[string]string{"primitive.yaml": manifest, "main.py": "tap.write(\"out/note.txt\", \"owned ACL fixture\")\n"} {
		if err := os.WriteFile(filepath.Join(primitive, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(work, "out", "note.txt")
	// The same released path must refuse before the test owner approves it.
	r := m.run(work, tap, "acl-output")
	if r.code == 0 {
		t.Fatal("unapproved owned write was accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("unapproved output exists: %v", err)
	}
	want(t, m.run(work, tap, "--approve", "acl-output"), 0, "RESULT (exit 0)")
	b, err := os.ReadFile(output)
	if err != nil || string(b) != "owned ACL fixture" {
		t.Fatalf("same-user output read: %q, %v", b, err)
	}
	windowsPrivateACL(t, filepath.Dir(output))
	windowsPrivateACL(t, output)
}
