package computenode

import "golang.org/x/sys/unix"

const (
	seccompArch = unix.AUDIT_ARCH_X86_64
	seccompX32  = true
)

var archDeniedSyscalls = []uintptr{unix.SYS_IOPL, unix.SYS_IOPERM, unix.SYS_MODIFY_LDT, unix.SYS_USELIB, unix.SYS_LOOKUP_DCOOKIE,
	// The old file calls the generic table (arm64) never had.
	unix.SYS_CHMOD, unix.SYS_CHOWN, unix.SYS_LCHOWN, unix.SYS_UTIME, unix.SYS_UTIMES, unix.SYS_FUTIMESAT}
