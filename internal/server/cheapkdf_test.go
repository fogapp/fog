package server

import (
	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/compute"
)

// Key stretching at full strength is seconds per call under -race, and
// is what these tests would spend most of their time on. The lowered
// costs hold only inside this test binary; the setters panic anywhere else.
func init() {
	backup.CheapKitKDFForTests()
	auth.CheapPasswordHashForTests()
	compute.CheapJoinKeyForTests()
}
