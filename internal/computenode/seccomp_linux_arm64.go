package computenode

import "golang.org/x/sys/unix"

const (
	seccompArch = unix.AUDIT_ARCH_AARCH64
	seccompX32  = false
)

var archDeniedSyscalls = []uintptr{unix.SYS_LOOKUP_DCOOKIE}
