//go:build !linux

package processguard

func verifyExactFencedProcessSignal(DurableIdentity) error {
	return ErrExactProcessSignalUnavailable
}

func requestExactFencedProcessStop(DurableIdentity) error {
	return ErrExactProcessSignalUnavailable
}
