package computenode

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// listProcessesArg makes this binary report what it can see of another
// process (lookAt).
const listProcessesArg = "-list-processes"

// meddleArg makes this binary try to change a file (meddle).
const meddleArg = "-meddle"

// fakeConfineArg makes this binary a helper that only pretends to
// confine.
const fakeConfineArg = "-fake-confine"

func TestMain(m *testing.M) {
	// A run's ffmpeg starts through this binary, as it does through
	// fog-node's own (Runner.Self).
	if len(os.Args) >= 2 && os.Args[1] == ConfineArg {
		err := ConfineMain(os.Args[2:])
		fmt.Fprintln(os.Stderr, "fog-node: not running ffmpeg, it couldn't be confined:", err)
		os.Exit(125)
	}
	if len(os.Args) >= 4 && os.Args[1] == fakeConfineArg && os.Args[2] == ConfineArg {
		var j jail
		json.Unmarshal([]byte(os.Args[3]), &j)
		// It does what a confining helper must for fog-node to carry on
		// (hand over the data socket, on Linux), and confines nothing.
		fakeHandOver(&j)
		j.Confine, j.RunAs = false, nil
		fmt.Fprintln(os.Stderr, confine(&j))
		os.Exit(125)
	}
	if len(os.Args) == 3 && os.Args[1] == listProcessesArg {
		// Run as a confined "ffmpeg": what can it see of another process?
		seen, args := lookAt(os.Args[2])
		fmt.Printf("listed:%v args:%v\n", seen, args)
		os.Exit(0)
	}
	if len(os.Args) == 3 && os.Args[1] == meddleArg {
		fmt.Println(meddle(os.Args[2]))
		os.Exit(0)
	}
	// Tests must not find a model server this machine really runs.
	knownUpstreams = []string{"http://127.0.0.1:1"}
	os.Exit(m.Run())
}
