// Package avatar generates the deterministic identity image written into
// every scaffolded agent: a glyph head with knocked-out eyes floating over
// elliptical shoulders, colored from the Steady palette.
//
// All geometry lives in a 96x96 unit space and every coordinate below is a
// fixed design decision from the approved mock, not a computed value. Colors
// are OKLCH ramp steps from the website's palette
// (steadyspacecorp/website, _source/assets/css/_base/@root.css), converted
// to gamut-clamped sRGB once and baked in.
package avatar

import (
	"crypto/sha256"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Generate returns the avatar for name as an SVG document and a 512px PNG.
// The same name always yields the same bytes.
func Generate(name string) (svg, png []byte, err error) {
	shapes, fills := scene(name)
	png, err = rasterize(shapes, fills, 512)
	if err != nil {
		return nil, nil, err
	}
	return renderSVG(shapes, fills), png, nil
}

type xform struct{ s, tx, ty float64 }

var identity = xform{1, 0, 0}

// compose returns the transform equivalent to applying a, then b.
func compose(a, b xform) xform {
	return xform{b.s * a.s, b.s*a.tx + b.tx, b.s*a.ty + b.ty}
}

type shape interface {
	contains(x, y float64) bool
	svg(fill string) string
	mapped(t xform) shape
}

type rect struct{ x, y, w, h, r float64 }

func (s rect) contains(x, y float64) bool {
	if x < s.x || x > s.x+s.w || y < s.y || y > s.y+s.h {
		return false
	}
	if s.r <= 0 {
		return true
	}
	cx := math.Max(s.x+s.r, math.Min(x, s.x+s.w-s.r))
	cy := math.Max(s.y+s.r, math.Min(y, s.y+s.h-s.r))
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= s.r*s.r
}

func (s rect) svg(fill string) string {
	if s.r > 0 {
		return fmt.Sprintf(`<rect x="%s" y="%s" width="%s" height="%s" rx="%s" fill="%s"/>`,
			num(s.x), num(s.y), num(s.w), num(s.h), num(s.r), fill)
	}
	return fmt.Sprintf(`<rect x="%s" y="%s" width="%s" height="%s" fill="%s"/>`,
		num(s.x), num(s.y), num(s.w), num(s.h), fill)
}

func (s rect) mapped(t xform) shape {
	return rect{t.s*s.x + t.tx, t.s*s.y + t.ty, t.s * s.w, t.s * s.h, t.s * s.r}
}

type circle struct{ cx, cy, r float64 }

func (s circle) contains(x, y float64) bool {
	dx, dy := x-s.cx, y-s.cy
	return dx*dx+dy*dy <= s.r*s.r
}

func (s circle) svg(fill string) string {
	return fmt.Sprintf(`<circle cx="%s" cy="%s" r="%s" fill="%s"/>`, num(s.cx), num(s.cy), num(s.r), fill)
}

func (s circle) mapped(t xform) shape {
	return circle{t.s*s.cx + t.tx, t.s*s.cy + t.ty, t.s * s.r}
}

type ellipse struct{ cx, cy, rx, ry float64 }

func (s ellipse) contains(x, y float64) bool {
	dx, dy := (x-s.cx)/s.rx, (y-s.cy)/s.ry
	return dx*dx+dy*dy <= 1
}

func (s ellipse) svg(fill string) string {
	return fmt.Sprintf(`<ellipse cx="%s" cy="%s" rx="%s" ry="%s" fill="%s"/>`,
		num(s.cx), num(s.cy), num(s.rx), num(s.ry), fill)
}

func (s ellipse) mapped(t xform) shape {
	return ellipse{t.s*s.cx + t.tx, t.s*s.cy + t.ty, t.s * s.rx, t.s * s.ry}
}

// halfDisc is a semicircle: the half of a disc above (or below) its center line.
type halfDisc struct {
	cx, cy, r float64
	down      bool
}

func (s halfDisc) contains(x, y float64) bool {
	if s.down && y < s.cy || !s.down && y > s.cy {
		return false
	}
	dx, dy := x-s.cx, y-s.cy
	return dx*dx+dy*dy <= s.r*s.r
}

func (s halfDisc) svg(fill string) string {
	sweep := 1
	if s.down {
		sweep = 0
	}
	return fmt.Sprintf(`<path d="M%s,%s a%s,%s 0 0 %d %s,0 Z" fill="%s"/>`,
		num(s.cx-s.r), num(s.cy), num(s.r), num(s.r), sweep, num(2*s.r), fill)
}

func (s halfDisc) mapped(t xform) shape {
	return halfDisc{t.s*s.cx + t.tx, t.s*s.cy + t.ty, t.s * s.r, s.down}
}

type poly struct{ pts [][2]float64 }

func (p poly) contains(x, y float64) bool {
	in := false
	for i, j := 0, len(p.pts)-1; i < len(p.pts); j, i = i, i+1 {
		xi, yi := p.pts[i][0], p.pts[i][1]
		xj, yj := p.pts[j][0], p.pts[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

func (p poly) svg(fill string) string {
	var b strings.Builder
	for i, pt := range p.pts {
		if i == 0 {
			b.WriteString("M")
		} else {
			b.WriteString(" L")
		}
		b.WriteString(num(pt[0]) + "," + num(pt[1]))
	}
	return fmt.Sprintf(`<path d="%s Z" fill="%s"/>`, b.String(), fill)
}

func (p poly) mapped(t xform) shape {
	pts := make([][2]float64, len(p.pts))
	for i, pt := range p.pts {
		pts[i] = [2]float64{t.s*pt[0] + t.tx, t.s*pt[1] + t.ty}
	}
	return poly{pts}
}

type head struct {
	name   string
	shapes []shape
	// nudge shifts the head toward the shoulders so every silhouette lands
	// the same visual distance above the shoulder crest.
	nudge float64
	// eye adjusts eye placement in head space for heads whose face sits off
	// the default center.
	eye xform
}

var heads = []head{
	{name: "cursor", nudge: 3, eye: identity, shapes: []shape{
		rect{22, 16, 52, 64, 0},
	}},
	{name: "arch", nudge: 1.5, eye: identity, shapes: []shape{
		rect{20, 46, 56, 36, 0},
		halfDisc{48, 46, 28, false},
	}},
	{name: "round", nudge: 2, eye: identity, shapes: []shape{
		circle{48, 48, 33},
	}},
	{name: "square", nudge: 3.5, eye: identity, shapes: []shape{
		rect{17, 17, 62, 62, 0},
	}},
	{name: "diamond", nudge: 1, eye: identity, shapes: []shape{
		poly{[][2]float64{{48, 11}, {83, 48}, {48, 85}, {13, 48}}},
	}},
	{name: "shield", nudge: 2.5, eye: identity, shapes: []shape{
		rect{19, 16, 58, 36, 0},
		halfDisc{48, 52, 29, true},
	}},
	{name: "hex", nudge: 0.5, eye: identity, shapes: []shape{
		poly{[][2]float64{{48, 10}, {82, 29.5}, {82, 66.5}, {48, 86}, {14, 66.5}, {14, 29.5}}},
	}},
	{name: "notch", nudge: 3, eye: identity, shapes: []shape{
		poly{[][2]float64{{20, 16}, {36, 16}, {36, 28}, {60, 28}, {60, 16}, {76, 16}, {76, 80}, {20, 80}}},
	}},
	{name: "keycap", nudge: 3, eye: xform{1, 0, 8}, shapes: []shape{
		rect{30, 14, 36, 26, 0},
		rect{17, 36, 62, 44, 0},
	}},
	{name: "tri", nudge: 2, eye: xform{0.8, 9.6, 23.6}, shapes: []shape{
		poly{[][2]float64{{48, 13}, {84, 81}, {12, 81}}},
	}},
}

type eyes struct {
	name   string
	shapes []shape
}

var eyeStyles = []eyes{
	{name: "dots", shapes: []shape{
		circle{36, 49, 5.5},
		circle{60, 49, 5.5},
	}},
	{name: "bars", shapes: []shape{
		rect{32.5, 38, 7, 20, 3.5},
		rect{56.5, 38, 7, 20, 3.5},
	}},
	{name: "px", shapes: []shape{
		rect{30, 43, 11, 11, 2},
		rect{55, 43, 11, 11, 2},
	}},
	{name: "arcs", shapes: []shape{
		halfDisc{38, 52, 8, false},
		halfDisc{58, 52, 8, false},
	}},
	{name: "dash", shapes: []shape{
		rect{30, 45, 12, 6, 0},
		rect{54, 45, 12, 6, 0},
	}},
	// caret and slash are pre-computed stroke outlines (chevron w5 miter,
	// slash w6 butt); polygons keep the rasterizer free of stroke math.
	{name: "caret", shapes: []shape{
		poly{[][2]float64{{27.12, 50.35}, {36, 40.2}, {44.88, 50.35}, {41.12, 53.65}, {36, 47.8}, {30.88, 53.65}}},
		poly{[][2]float64{{51.12, 50.35}, {60, 40.2}, {68.88, 50.35}, {65.12, 53.65}, {60, 47.8}, {54.88, 53.65}}},
	}},
	{name: "slash", shapes: []shape{
		poly{[][2]float64{{30.24, 53.82}, {36.24, 39.82}, {41.76, 42.18}, {35.76, 56.18}}},
		poly{[][2]float64{{54.24, 53.82}, {60.24, 39.82}, {65.76, 42.18}, {59.76, 56.18}}},
	}},
}

type palette struct{ bg, fg, body string }

type hue struct {
	name        string
	light, dark palette
}

var hues = []hue{
	{"ultramarine",
		palette{"#d8e0ff", "#4738f2", "#2f11b6"},
		palette{"#171361", "#b7c4ff", "#7484ff"}},
	{"aqua",
		palette{"#bceceb", "#007272", "#004d4d"},
		palette{"#002b2b", "#6fdcdb", "#45a3a3"}},
	{"grape",
		palette{"#e6daff", "#7d24d3", "#57009b"},
		palette{"#2f0b54", "#d1b9ff", "#a670f4"}},
	{"sky",
		palette{"#cbe4ff", "#0065b0", "#004479"},
		palette{"#002546", "#9cccff", "#5497d9"}},
	{"lemon",
		palette{"#f4e19b", "#766200", "#4f4100"},
		palette{"#2c2300", "#eac500", "#ac9000"}},
	{"orange",
		palette{"#ffd7cf", "#ba0d01", "#800500"},
		palette{"#490402", "#ffb0a3", "#e36654"}},
	{"rhubarb",
		palette{"#ffd5da", "#b80049", "#7e002f"},
		palette{"#480219", "#ffadb9", "#e1627c"}},
}

const bustScale = 0.8

func scene(name string) (shapes []shape, fills []string) {
	sum := sha256.Sum256([]byte(name))
	h := heads[int(sum[0])%len(heads)]
	e := eyeStyles[int(sum[1])%len(eyeStyles)]
	hu := hues[int(sum[2])%len(hues)]
	pal := hu.light
	if sum[3]%2 == 1 {
		pal = hu.dark
	}

	add := func(s shape, fill string) {
		shapes = append(shapes, s)
		fills = append(fills, fill)
	}

	add(rect{0, 0, 96, 96, 0}, pal.bg)
	add(ellipse{48, 114, 44, 37}, pal.body)

	headY := 40 + h.nudge
	bust := xform{bustScale, 48 * (1 - bustScale), headY - 48*bustScale}
	for _, s := range h.shapes {
		add(s.mapped(bust), pal.fg)
	}
	eyeXF := compose(h.eye, bust)
	for _, s := range e.shapes {
		add(s.mapped(eyeXF), pal.bg)
	}
	return shapes, fills
}

func renderSVG(shapes []shape, fills []string) []byte {
	var b strings.Builder
	b.WriteString("<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 96 96\">\n")
	for i, s := range shapes {
		b.WriteString("  " + s.svg(fills[i]) + "\n")
	}
	b.WriteString("</svg>\n")
	return []byte(b.String())
}

func num(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}
