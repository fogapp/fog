package hostcpu

import "testing"

// The fog VM's own /proc/cpuinfo, as of the BookOrbit crash: a kvm64 model
// that stops at SSE2 — the case this package exists for.
const kvm64 = `processor	: 0
vendor_id	: GenuineIntel
model name	: Common KVM processor
flags		: fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat pse36 clflush mmx fxsr sse sse2 ht syscall nx lm constant_tsc nopl xtopology cpuid tsc_known_freq pni cx16 x2apic hypervisor lahf_lm cpuid_fault pti
processor	: 1
model name	: Common KVM processor
flags		: fpu sse sse2
`

const haswell = `model name	: Intel(R) Core(TM) i7-4770 CPU @ 3.40GHz
flags		: fpu sse sse2 ssse3 cx16 sse4_1 sse4_2 popcnt lahf_lm avx avx2 bmi1 bmi2 f16c fma abm movbe xsave
`

const skylakeX = `model name	: Intel(R) Xeon(R) Platinum
flags		: fpu sse sse2 ssse3 cx16 sse4_1 sse4_2 popcnt lahf_lm avx avx2 bmi1 bmi2 f16c fma abm movbe xsave avx512f avx512bw avx512cd avx512dq avx512vl hypervisor
`

func TestParseLevels(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		level   int
		virtual bool
		model   string
	}{
		{"kvm64", kvm64, 1, true, "Common KVM processor"},
		{"haswell", haswell, 3, false, "Intel(R) Core(TM) i7-4770 CPU @ 3.40GHz"},
		{"skylake-x", skylakeX, 4, true, "Intel(R) Xeon(R) Platinum"},
		{"nehalem", "flags: sse2 ssse3 cx16 sse4_1 sse4_2 popcnt lahf_lm\n", 2, false, ""},
	}
	for _, c := range cases {
		got := Parse(c.in, "amd64")
		if !got.Known {
			t.Errorf("%s: flags present but Known=false", c.name)
		}
		if got.Level != c.level || got.Virtual != c.virtual || got.Model != c.model {
			t.Errorf("%s: got %+v, want level %d virtual %v model %q", c.name, got, c.level, c.virtual, c.model)
		}
	}
}

// Without a flags line nothing is known — and unknown must never be
// reported as level 1, which would block installs on a box we simply
// couldn't read.
func TestParseUnknown(t *testing.T) {
	got := Parse("model name: something\n", "amd64")
	if got.Known || got.Level != 0 {
		t.Errorf("got %+v, want unknown", got)
	}
}

// Levels are an x86-64 notion; an arm64 host reads its flags (so Known and
// Virtual are honest) but has no level for a requirement to fall short of.
func TestParseArm64(t *testing.T) {
	got := Parse("model name: Cortex-A72\nFeatures: fp asimd\nflags: hypervisor\n", "arm64")
	if !got.Known || got.Level != 0 || !got.Virtual {
		t.Errorf("got %+v, want known, level 0, virtual", got)
	}
}
