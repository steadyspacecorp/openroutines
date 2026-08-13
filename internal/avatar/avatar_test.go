package avatar

import (
	"bytes"
	"flag"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestGenerateIsDeterministic(t *testing.T) {
	svg1, png1, err := Generate("product-assistant")
	if err != nil {
		t.Fatal(err)
	}
	svg2, png2, err := Generate("product-assistant")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(svg1, svg2) || !bytes.Equal(png1, png2) {
		t.Fatal("same name produced different avatars")
	}
}

func TestGenerateGolden(t *testing.T) {
	names := []string{
		"product-assistant", "engineering-assistant", "knowledge-librarian",
		"market-scout", "deploy-sentry", "changelog-scribe", "inbox-triager",
		"release-drummer",
	}
	for _, name := range names {
		svg, _, err := Generate(name)
		if err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("testdata", name+".svg")
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, svg, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(svg, want) {
			t.Errorf("%s: SVG differs from golden file %s (run with -update to accept)", name, golden)
		}
	}
}

func TestPNGMatchesScene(t *testing.T) {
	const name = "product-assistant"
	_, data, err := Generate(name)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := img.Bounds().Dx(); got != 512 {
		t.Fatalf("width = %d, want 512", got)
	}
	_, fills := scene(name)
	want, err := parseHex(fills[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := color.RGBAModel.Convert(img.At(2, 2)); got != want {
		t.Fatalf("corner pixel = %v, want background %v", got, want)
	}
}
