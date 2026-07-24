//go:build !linux

package processguard

import "os"

func validateDurableReceiptConfigPlatform(_ *os.File) error {
	return nil
}
