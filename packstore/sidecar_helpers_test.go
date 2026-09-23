package packstore

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"testing"
)

// resealSidecarRec recomputes the CRC of a hand-damaged record, so that a
// test reaches the checks behind it.
func resealSidecarRec(b [sidecarRecSize]byte) []byte {
	binary.BigEndian.PutUint32(b[52:56], crc32.Checksum(b[:52], castagnoli))
	return b[:]
}

func readSidecarFile(t *testing.T, path string) ([]sidecarRec, int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return readSidecar(b, true)
}
