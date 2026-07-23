//go:build windows

package filesecurity

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type aclHeader struct {
	revision  byte
	reserved  byte
	size      uint16
	aceCount  uint16
	reserved2 uint16
}

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current Windows user: %w", err)
	}
	if user == nil || user.User.Sid == nil {
		return nil, errors.New("current Windows user has no SID")
	}
	return user.User.Sid, nil
}

func restrictPathToCurrentUser(path string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("create current-user Windows DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		sid,
		nil,
		acl,
		nil,
	); err != nil {
		return fmt.Errorf("protect file with current-user Windows DACL: %w", err)
	}
	return nil
}

// RestrictToCurrentUser replaces inherited access with one protected
// current-user ACE. Windows does not implement Unix 0600 semantics through
// os.Chmod, so the handoff uses a user-scoped DACL instead.
func RestrictToCurrentUser(path string) error {
	return restrictPathToCurrentUser(path)
}

// RestrictDirectoryToCurrentUser protects the manifest parent from rename and
// delete-child attacks by other ordinary Windows users.
func RestrictDirectoryToCurrentUser(path string) error {
	return restrictPathToCurrentUser(path)
}

// ValidateCurrentUserFile rejects reparse points and requires the exact
// protected DACL written by RestrictToCurrentUser. Administrators can still
// exercise their platform privileges, but ordinary users and inherited groups
// receive no file access.
func ValidateCurrentUserFile(path string, info os.FileInfo) error {
	if info == nil {
		return errors.New("file metadata is missing")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("file is not a regular non-link file")
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil {
		return fmt.Errorf("read Windows file attributes: %w", err)
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("file must not be a Windows reparse point")
	}
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("read Windows file security: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read Windows file owner: %w", err)
	}
	if owner == nil || !windows.EqualSid(owner, sid) {
		return errors.New("Windows file owner is not the current user")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read Windows DACL control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("Windows file DACL inherits access")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read Windows file DACL: %w", err)
	}
	if dacl == nil {
		return errors.New("Windows file DACL is empty")
	}
	header := (*aclHeader)(unsafe.Pointer(dacl))
	if header.aceCount != 1 {
		return fmt.Errorf("Windows file DACL has %d entries, want 1", header.aceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return fmt.Errorf("read Windows file DACL entry: %w", err)
	}
	if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
		ace.Mask != windows.GENERIC_ALL {
		return errors.New("Windows file DACL is not a current-user full-access entry")
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !windows.EqualSid(aceSID, sid) {
		return errors.New("Windows file DACL grants access outside the current user")
	}
	return nil
}
