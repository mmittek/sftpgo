// Text on the audio card video (fork addition): the file name on top and the details at the
// bottom, in Dogica Pixel by Roberto Mocci, under the SIL Open Font License (fonts/OFL.txt).

package httpd

import (
	_ "embed"
	"image"
	"image/color"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	//go:embed fonts/dogicapixel.ttf
	cardFontData []byte
	//go:embed fonts/dogicapixelbold.ttf
	cardBoldFontData []byte

	cardFontOnce       sync.Once
	cardFont, cardBold *opentype.Font
)

// cardFace returns the pixel font at a size, nil if it cannot be loaded. Sizes in multiples
// of 8 keep the pixels on the grid
func cardFace(size float64, bold bool) font.Face {
	cardFontOnce.Do(func() {
		cardFont, _ = opentype.Parse(cardFontData)
		cardBold, _ = opentype.Parse(cardBoldFontData)
	})
	f := cardFont
	if bold {
		f = cardBold
	}
	if f == nil {
		return nil
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil
	}
	return face
}

// drawCardText writes text at a baseline, cut with "..." to fit maxWidth
func drawCardText(dst *image.RGBA, text string, size float64, bold bool, x, baseline, maxWidth int, c color.RGBA) {
	face := cardFace(size, bold)
	if face == nil {
		return
	}
	defer face.Close()
	d := &font.Drawer{Dst: dst, Src: image.NewUniform(c), Face: face}
	limit := fixed.I(maxWidth)
	if d.MeasureString(text) > limit {
		runes := []rune(text)
		for len(runes) > 0 && d.MeasureString(string(runes)+"...") > limit {
			runes = runes[:len(runes)-1]
		}
		text = string(runes) + "..."
	}
	d.Dot = fixed.P(x, baseline)
	d.DrawString(text)
}
