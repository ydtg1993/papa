package fetchruntime

import (
	"reflect"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
)

func TestCloneDownloadsOwnsEveryOverlayField(t *testing.T) {
	if got := CloneDownloads(SiteDownloads{}); got.M3U8 != nil || got.FileDown != nil {
		t.Fatal("absent overlays became present")
	}
	in := SiteDownloads{M3U8: &m3u8.Overlay{}, FileDown: &filedown.Overlay{}}
	empty := CloneDownloads(in)
	if empty.M3U8 == in.M3U8 || empty.FileDown == in.FileDown || !empty.M3U8.IsZero() || !empty.FileDown.IsZero() {
		t.Fatal("empty overlay ownership or nil field semantics changed")
	}
	// Cover all pointer fields, including future overlay additions and explicit
	// zero/false values. No registration may keep a pointer into caller state.
	for _, overlay := range []any{in.M3U8, in.FileDown} {
		v := reflect.ValueOf(overlay).Elem()
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if field.Kind() != reflect.Pointer {
				t.Fatalf("review new overlay field ownership: %s", v.Type().Field(i).Name)
			}
			field.Set(reflect.New(field.Type().Elem()))
		}
	}
	out := CloneDownloads(in)
	for _, pair := range [][2]any{{in.M3U8, out.M3U8}, {in.FileDown, out.FileDown}} {
		src, dst := reflect.ValueOf(pair[0]).Elem(), reflect.ValueOf(pair[1]).Elem()
		for i := 0; i < src.NumField(); i++ {
			a, b := src.Field(i), dst.Field(i)
			if b.IsNil() || a.Pointer() == b.Pointer() || !reflect.DeepEqual(a.Interface(), b.Interface()) {
				t.Errorf("%s.%s lost value or aliases caller state", src.Type().Name(), src.Type().Field(i).Name)
			}
		}
	}
}
