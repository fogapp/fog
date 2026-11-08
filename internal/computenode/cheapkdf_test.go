package computenode

import (
	"gitea.lan/matthew/fog/internal/compute"
)

// Key stretching at full strength is seconds per call under -race, and
// is what these tests would spend most of their time on. The lowered
// costs hold only inside this test binary; the setters panic anywhere else.
func init() {
	compute.CheapJoinKeyForTests()
}
