//go:build ignore

// Generate the simple geometric app mark; no external image dependencies.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
)

func main() {
	for _, n := range []int{192, 512} {
		im := image.NewRGBA(image.Rect(0, 0, n, n))
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				a, b := float64(x)*512/float64(n), float64(y)*512/float64(n)
				c := color.RGBA{247, 246, 243, 255}
				if (a+b > 430 && a+b < 550 && b-a < 120 && a-b < 205 && a > 144 && b < 352) || (b > 382 && b < 398 && a > 144 && a < 380) {
					c = color.RGBA{138, 75, 0, 255}
				}
				im.SetRGBA(x, y, c)
			}
		}
		f, e := os.Create(fmt.Sprintf("internal/web/icon-%d.png", n))
		if e != nil {
			panic(e)
		}
		if e = png.Encode(f, im); e != nil {
			panic(e)
		}
		f.Close()
	}
}
