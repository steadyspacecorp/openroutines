package avatar

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

func rasterize(shapes []shape, fills []string, size int) ([]byte, error) {
	colors := make([]color.RGBA, len(fills))
	for i, f := range fills {
		c, err := parseHex(f)
		if err != nil {
			return nil, err
		}
		colors[i] = c
	}

	const sub = 4
	scale := float64(size) / 96
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b int
			for sy := 0; sy < sub; sy++ {
				for sx := 0; sx < sub; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/sub) / scale
					y := (float64(py) + (float64(sy)+0.5)/sub) / scale
					c := colors[0]
					for i := len(shapes) - 1; i > 0; i-- {
						if shapes[i].contains(x, y) {
							c = colors[i]
							break
						}
					}
					r += int(c.R)
					g += int(c.G)
					b += int(c.B)
				}
			}
			n := sub * sub
			img.SetRGBA(px, py, color.RGBA{uint8(r / n), uint8(g / n), uint8(b / n), 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseHex(s string) (color.RGBA, error) {
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b); err != nil {
		return color.RGBA{}, fmt.Errorf("bad color %q: %w", s, err)
	}
	return color.RGBA{r, g, b, 255}, nil
}
