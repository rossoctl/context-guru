package offload_test

import (
	"bufio"
	"io"
)

func newScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 1<<20), 256<<20)
	return s
}
