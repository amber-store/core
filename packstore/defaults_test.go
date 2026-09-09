package packstore

import "testing"

func TestDefaultSegmentSize(t *testing.T) {
	if DefaultSegmentSize != 2<<30 {
		t.Fatalf("DefaultSegmentSize = %d, want 2 GiB", DefaultSegmentSize)
	}
}
