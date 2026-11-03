//go:build !linux

package setup

// The live installer only ever runs on Linux; these stubs keep the package
// (and its mock mode, which is how the wizard is developed on a Mac)
// building everywhere else.
func bootDiskName() string                           { return "" }
func readInstallMarker(espDev string) *InstallMarker { return nil }
func liveNetwork() Network                           { return Network{} }
func dmiProductName() string                         { return "" }
