package server

import (
	"io/fs"

	"github.com/ydtg1993/oao"
)

// Published older oao releases remain usable. Only reference assets that are
// actually embedded in the linked dependency, never a guessed future version.
func optionalOaoAssets() (charts, motion bool) {
	assets, err := new(oao.Oao).StaticFS()
	if err != nil {
		return false, false
	}
	return hasVisualAssets(assets, "oao-charts"), hasVisualAssets(assets, "oao-motion")
}

func hasVisualAssets(assets fs.FS, name string) bool {
	for _, ext := range []string{".js", ".css"} {
		info, err := fs.Stat(assets, name+ext)
		if err != nil || info.IsDir() || info.Size() == 0 {
			return false
		}
	}
	return true
}
