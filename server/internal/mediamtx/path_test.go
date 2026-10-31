package mediamtx_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/mediamtx"
)

func TestParsePath(t *testing.T) {
	id := uuid.MustParse("01900000-0000-7000-8000-0000000000c1")
	if got, sub, ok := mediamtx.ParsePath(mediamtx.MainPath(id)); !ok || sub || got != id {
		t.Errorf("main: %v %v %v", got, sub, ok)
	}
	if got, sub, ok := mediamtx.ParsePath(mediamtx.SubPath(id)); !ok || !sub || got != id {
		t.Errorf("sub: %v %v %v", got, sub, ok)
	}
	for _, bad := range []string{
		"",
		"cam_",
		"cam__main",
		"testsrc",
		"cam_smoke_main",
		"cam_01900000-0000-7000-8000-0000000000c1",
		"cam_01900000-0000-7000-8000-0000000000c1_other",
		"cam_01900000-0000-7000-8000-0000000000C1_main",          // upper case
		"cam_{01900000-0000-7000-8000-0000000000c1}_main",        // braces
		"cam_urn:uuid:01900000-0000-7000-8000-0000000000c1_main", // urn
		"cam_019000000000700080000000000000c1_main",              // no hyphens
		"CAM_01900000-0000-7000-8000-0000000000c1_main",
		"cam_01900000-0000-7000-8000-0000000000c1_main/x",
		"x/cam_01900000-0000-7000-8000-0000000000c1_main",
		"cam_01900000-0000-7000-8000-0000000000c1_main_sub",
	} {
		if _, _, ok := mediamtx.ParsePath(bad); ok {
			t.Errorf("ParsePath(%q) accepted", bad)
		}
	}
}
