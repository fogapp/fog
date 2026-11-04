package chunkstore

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand/v2"
	"testing"
)

var smallParams = Params{Min: 2 << 10, Avg: 8 << 10, Max: 32 << 10}

func randBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func chunksOf(t *testing.T, data []byte, g *Gear, p Params) [][]byte {
	t.Helper()
	c, err := NewChunker(bytes.NewReader(data), g, p)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for {
		b, err := c.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append([]byte(nil), b...))
	}
}

func TestChunkerReassemblesAndBounds(t *testing.T) {
	data := randBytes(1, 3<<20)
	chunks := chunksOf(t, data, NewGear([]byte("k")), smallParams)
	if got := bytes.Join(chunks, nil); !bytes.Equal(got, data) {
		t.Fatal("chunks do not reassemble the stream")
	}
	for i, c := range chunks {
		if len(c) > smallParams.Max {
			t.Fatalf("chunk %d is %d bytes, past max", i, len(c))
		}
		if i < len(chunks)-1 && len(c) < smallParams.Min {
			t.Fatalf("chunk %d is %d bytes, under min", i, len(c))
		}
	}
	avg := len(data) / len(chunks)
	if avg < smallParams.Avg/2 || avg > smallParams.Avg*2 {
		t.Fatalf("average chunk %d bytes, far from %d", avg, smallParams.Avg)
	}
}

func TestChunkerEmptyAndTiny(t *testing.T) {
	if c := chunksOf(t, nil, nil, smallParams); len(c) != 0 {
		t.Fatalf("empty stream gave %d chunks", len(c))
	}
	if c := chunksOf(t, []byte("hi"), nil, smallParams); len(c) != 1 || string(c[0]) != "hi" {
		t.Fatalf("tiny stream gave %q", c)
	}
}

// An insertion near the front must leave the rest of the stream's chunks
// as they were — the property every saving depends on.
func TestChunkerShiftResilience(t *testing.T) {
	g := NewGear([]byte("k"))
	data := randBytes(2, 2<<20)
	edited := append(append(append([]byte{}, data[:1000]...), []byte("inserted bytes")...), data[1000:]...)
	set := map[[32]byte]bool{}
	for _, c := range chunksOf(t, data, g, smallParams) {
		set[sha256.Sum256(c)] = true
	}
	after := chunksOf(t, edited, g, smallParams)
	shared := 0
	for _, c := range after {
		if set[sha256.Sum256(c)] {
			shared++
		}
	}
	if shared < len(after)-3 {
		t.Fatalf("only %d of %d chunks survived a 14-byte insertion", shared, len(after))
	}
}

// Different seeds cut in different places: boundaries are a secret of the
// store.
func TestChunkerKeyed(t *testing.T) {
	data := randBytes(3, 1<<20)
	a := chunksOf(t, data, NewGear([]byte("a")), smallParams)
	b := chunksOf(t, data, NewGear([]byte("b")), smallParams)
	same := len(a) == len(b)
	for i := 0; same && i < len(a); i++ {
		same = len(a[i]) == len(b[i])
	}
	if same {
		t.Fatal("two seeds produced identical boundaries")
	}
}

func TestParamsValidate(t *testing.T) {
	for _, p := range []Params{{0, 8, 16}, {8, 8, 16}, {4, 12, 32}, {4, 8, 8}} {
		if _, err := NewChunker(nil, nil, p); err == nil {
			t.Fatalf("params %+v accepted", p)
		}
	}
}
