// Command screenshot-critique captures every view with web/e2e/shots.mjs, then
// makes the two copies the design critique looks at: a blurred one (what still
// dominates when you squint) and a half-size one (what stays legible from the
// back of the room). index.html shows them side by side with each screen's
// 5-second message. A person, or a fresh reviewer, does the judging.
package main

import (
	"fmt"
	"html"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const out = "artifacts/screenshots"

var messages = map[string]string{
	"monitor":   "The main database is safe under this load, and here is every PRD target.",
	"store":     "How my store did in this period, and it is live.",
	"company":   "All my stores together and each one separately.",
	"isolation": "No store's answer can reach another store, and the drill just proved it.",
	"bench":     "What we measured, including what we did not hit.",
}

// shrink box-averages src down by an integer factor.
func shrink(src image.Image, f int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx()/f, b.Dy()/f
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, bl uint32
			for j := 0; j < f; j++ {
				for i := 0; i < f; i++ {
					cr, cg, cb, _ := src.At(b.Min.X+x*f+i, b.Min.Y+y*f+j).RGBA()
					r, g, bl = r+cr>>8, g+cg>>8, bl+cb>>8
				}
			}
			n := uint32(f * f)
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), 255})
		}
	}
	return dst
}

// grow scales src up by an integer factor, blending neighbours.
func grow(src *image.RGBA, f int) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx()*f, b.Dy()*f))
	at := func(x, y int) color.RGBA {
		return src.RGBAAt(min(max(x, 0), b.Dx()-1), min(max(y, 0), b.Dy()-1))
	}
	lerp := func(a, c uint8, t float64) uint8 { return uint8(float64(a)*(1-t) + float64(c)*t) }
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < dst.Bounds().Dx(); x++ {
			fx, fy := float64(x)/float64(f), float64(y)/float64(f)
			x0, y0 := int(fx), int(fy)
			tx, ty := fx-float64(x0), fy-float64(y0)
			p, q, r, s := at(x0, y0), at(x0+1, y0), at(x0, y0+1), at(x0+1, y0+1)
			mix := func(a, c, d, e uint8) uint8 { return lerp(lerp(a, c, tx), lerp(d, e, tx), ty) }
			dst.SetRGBA(x, y, color.RGBA{mix(p.R, q.R, r.R, s.R), mix(p.G, q.G, r.G, s.G), mix(p.B, q.B, r.B, s.B), 255})
		}
	}
	return dst
}

func save(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "screenshot-critique:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 || os.Args[1] != "--no-capture" {
		cmd := exec.Command("node", "e2e/shots.mjs", filepath.Join("..", out))
		cmd.Dir = "web"
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("capture: %w", err)
		}
	}
	for _, d := range []string{"squint", "half"} {
		if err := os.MkdirAll(filepath.Join(out, d), 0o755); err != nil {
			return err
		}
	}
	files, _ := filepath.Glob(filepath.Join(out, "*.png"))
	sort.Strings(files)
	var page strings.Builder
	page.WriteString(`<!doctype html><meta charset="utf-8"><title>Screenshot critique</title>
<style>body{font:16px system-ui;margin:24px}figure{margin:0 0 8px}img{max-width:100%;border:1px solid #bbb}.row{display:grid;grid-template-columns:2fr 1fr 1fr;gap:12px;margin-bottom:32px}</style>
<h1>Screenshot critique</h1><p>For each screen: does the message land in 5 seconds? When blurred, do the recorder, the scorecard and the summary figures still dominate? At half size, can you read the labels and key numbers?</p>`)
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".png")
		if strings.HasSuffix(name, "-full") || strings.HasPrefix(name, "autopilot") {
			continue
		}
		in, err := os.Open(f)
		if err != nil {
			return err
		}
		img, err := png.Decode(in)
		in.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		if err := save(filepath.Join(out, "half", name+".png"), shrink(img, 2)); err != nil {
			return err
		}
		if err := save(filepath.Join(out, "squint", name+".png"), grow(shrink(img, 10), 10)); err != nil {
			return err
		}
		msg := ""
		for k, v := range messages {
			if strings.HasPrefix(name, k) {
				msg = v
			}
		}
		fmt.Fprintf(&page, `<h2>%s</h2><p>%s</p><div class="row"><img src="%s.png" alt=""><img src="squint/%s.png" alt=""><img src="half/%s.png" alt=""></div>`,
			html.EscapeString(name), html.EscapeString(msg), name, name, name)
	}
	idx := filepath.Join(out, "index.html")
	if err := os.WriteFile(idx, []byte(page.String()), 0o644); err != nil {
		return err
	}
	fmt.Println("open", idx)
	return nil
}
