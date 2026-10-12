package credmask

import (
	"bytes"
	"testing"
)

func TestMaskIsItsOwnInverse(t *testing.T) {
	orig := []byte("sk-ant-api03-secret-value")
	b := append([]byte(nil), orig...)
	Mask(b)
	if bytes.Equal(b, orig) {
		t.Fatal("masked bytes equal the plaintext")
	}
	if bytes.Contains(b, []byte("secret")) {
		t.Fatal("masked bytes still contain a plaintext run")
	}
	Mask(b)
	if !bytes.Equal(b, orig) {
		t.Fatalf("unmask = %q, want %q", b, orig)
	}
}

func TestZeroOverwrites(t *testing.T) {
	b := []byte("Bearer abc")
	Zero(b)
	for i, c := range b {
		if c != 0 {
			t.Fatalf("byte %d = %d after Zero, want 0", i, c)
		}
	}
}

func TestHeaderCloneIsIndependent(t *testing.T) {
	h := Header{Name: "Authorization", Val: []byte("x")}
	c := h.Clone()
	Zero(h.Val)
	if c.Val[0] != 'x' {
		t.Fatal("Clone shares its value with the original")
	}
}
