//go:build windows

package history

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func assertPrivateCacheDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("cache directory: %v %v, want a directory", info, err)
	}
	// FILE_READ_DATA/WRITE_DATA also represent directory listing/creation
	// rights, so the same native broad-group DACL check applies here.
	assertPrivateCacheFile(t, path)
}

// Windows permissions are ACLs, not POSIX mode bits. Check that the cache
// file's native DACL does not grant content or permission access to broad
// groups; Go's Mode().Perm() cannot establish this property on Windows.
func assertPrivateCacheFile(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		t.Fatalf("native security descriptor for %s: %v, %v", path, sd, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("%s has no restricting native DACL: %v", path, err)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("unhandled native ACL entry type %d; cannot establish cache privacy", ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		broad := sid.IsWellKnown(windows.WinWorldSid) || sid.IsWellKnown(windows.WinAuthenticatedUserSid) || sid.IsWellKnown(windows.WinBuiltinUsersSid)
		access := uint32(ace.Mask)
		privateRights := uint32(windows.GENERIC_ALL | windows.GENERIC_READ | windows.GENERIC_WRITE | windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.WRITE_DAC | windows.WRITE_OWNER)
		if broad && access&privateRights != 0 {
			t.Errorf("cache file %s grants broad group %s content or permission access: %#x", path, sid.String(), access)
		}
	}
}
