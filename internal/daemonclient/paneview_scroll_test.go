package daemonclient

import (
	"testing"
)

func TestScrollLines(t *testing.T) {
	tests := []struct {
		name          string
		initialOffset int
		dir           int
		wantOffset    int
		wantDirty     bool
	}{
		{"down decrements", 5, -1, 4, true},
		{"down clamps at 0", 0, -1, 0, true},
		{"up with nil vt is no-op", 0, 1, 0, false},
		{"zero dir is no-op", 3, 0, 3, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv := &PaneView{viewportOffset: tt.initialOffset}
			pv.ScrollLines(tt.dir)
			if got := pv.viewportOffset; got != tt.wantOffset {
				t.Errorf("viewportOffset = %d, want %d", got, tt.wantOffset)
			}
			if pv.dirty != tt.wantDirty {
				t.Errorf("dirty = %v, want %v", pv.dirty, tt.wantDirty)
			}
		})
	}
}
