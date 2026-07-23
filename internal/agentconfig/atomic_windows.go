//go:build windows

package agentconfig

import "golang.org/x/sys/windows"

func replaceFile(source, target string) error {
	return windows.Rename(source, target)
}

func syncDirectory(string) error {
	return nil
}
