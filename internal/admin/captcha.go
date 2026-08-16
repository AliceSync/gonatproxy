package admin

// Image-based CAPTCHA generated with the standard library only. A random code
// is drawn onto a PNG plus noise lines/dots. Verification is case-insensitive.

import (
	"bytes"
	"crypto/rand"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math/big"
	"sync"
	"time"
)

const validChars = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no 0/O/1/I/L ambiguity

type captcha struct {
	mu    sync.Mutex
	codes map[string]*captchaEntry
	ttl   time.Duration
}

type captchaEntry struct {
	code string
	exp  time.Time
}

func newCaptcha(ttl time.Duration) *captcha {
	return &captcha{codes: map[string]*captchaEntry{}, ttl: ttl}
}

// New generates a captcha and returns its token and PNG bytes.
func (c *captcha) New() (token string, pngBytes []byte, err error) {
	code := randomCode(5)
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", nil, err
	}
	token = hexEncode(tokenBytes)
	c.mu.Lock()
	c.codes[token] = &captchaEntry{code: code, exp: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	img := renderCaptcha(code)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", nil, err
	}
	return token, buf.Bytes(), nil
}

// Verify checks a submitted answer for a token and consumes it (one-use).
func (c *captcha) Verify(token, answer string) bool {
	c.mu.Lock()
	e, ok := c.codes[token]
	if ok {
		delete(c.codes, token)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	if time.Now().After(e.exp) {
		return false
	}
	return eqFold(answer, e.code)
}

func randomCode(n int) string {
	b := make([]byte, n)
	for i := range b {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(len(validChars))))
		b[i] = validChars[j.Int64()]
	}
	return string(b)
}

func hexEncode(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out)
}

func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'a' <= ca && ca <= 'z' {
			ca -= 'a' - 'A'
		}
		if 'a' <= cb && cb <= 'z' {
			cb -= 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func renderCaptcha(code string) image.Image {
	w, h := 160, 60
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{245, 245, 248, 255}}, image.Point{}, draw.Src)

	// noise background dots
	for i := 0; i < 220; i++ {
		x := randN(w)
		y := randN(h)
		c := color.RGBA{uint8(randN(180)), uint8(randN(180)), uint8(randN(220)), 255}
		img.Set(x, y, c)
	}
	// random curved lines
	for l := 0; l < 3; l++ {
		col := color.RGBA{uint8(randN(200)), uint8(randN(200)), uint8(randN(200)), 255}
		prev := image.Pt(0, randN(h))
		step := w / 24
		for x := step; x < w; x += step {
			cur := image.Pt(x, prev.Y+(randN(14)-7))
			drawLine(img, prev, cur, col)
			prev = cur
		}
	}
	// characters
	x := 14
	for _, r := range code {
		drawChar(img, x, 8, byte(r), color.RGBA{30, 30, 60, 255})
		x += 28
	}
	return img
}

func randN(n int) int {
	j, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	return int(j.Int64())
}

func drawLine(img *image.RGBA, a, b image.Point, col color.Color) {
	steep := abs(b.Y-a.Y) > abs(b.X-a.X)
	if steep {
		a.X, a.Y = a.Y, a.X
		b.X, b.Y = b.Y, b.X
	}
	if a.X > b.X {
		a, b = b, a
	}
	dx, dy := b.X-a.X, abs(b.Y-a.Y)
	err := dx / 2
	y := a.Y
	for x := a.X; x <= b.X; x++ {
		if steep {
			img.Set(y, x, col)
		} else {
			img.Set(x, y, col)
		}
		err -= dy
		if err < 0 {
			y++
			err += dx
		}
	}
}

func drawChar(img *image.RGBA, x, y int, c byte, col color.Color) {
	// 5x7 pixel font mapping for unambiguous captcha characters
	font := imgGlyphs()
	g, ok := font[rune(c)]
	if !ok {
		return
	}
	col2 := shade(col)
	for j := 0; j < 7; j++ {
		row := g[j]
		for i := 0; i < 5; i++ {
			if row&(1<<uint(i)) == 0 {
				continue
			}
			px, py := x+i*2, y+j*2
			img.Set(px, py, col)
			img.Set(px+1, py, col2)
			img.Set(px, py+1, col2)
			img.Set(px+1, py+1, col)
		}
	}
}

func shade(c color.Color) color.Color {
	r, g, b, _ := c.RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 255}
}

// imgGlyphs maps a captcha char to 7 rows; bit i (0..4) set = pixel at column i.
func imgGlyphs() map[rune][7]byte {
	return map[rune][7]byte{
		'A': {0b01110, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
		'B': {0b11110, 0b10001, 0b10001, 0b11110, 0b10001, 0b10001, 0b11110},
		'C': {0b01110, 0b10001, 0b10000, 0b10000, 0b10000, 0b10001, 0b01110},
		'D': {0b11110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b11110},
		'E': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b11111},
		'F': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b10000},
		'G': {0b01110, 0b10001, 0b10000, 0b10111, 0b10001, 0b10001, 0b01111},
		'H': {0b10001, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
		'J': {0b00111, 0b00001, 0b00001, 0b00001, 0b00001, 0b10001, 0b01110},
		'K': {0b10001, 0b10010, 0b10100, 0b11000, 0b10100, 0b10010, 0b10001},
		'M': {0b10001, 0b11011, 0b10101, 0b10101, 0b10001, 0b10001, 0b10001},
		'N': {0b10001, 0b11001, 0b10101, 0b10011, 0b10001, 0b10001, 0b10001},
		'P': {0b11110, 0b10001, 0b10001, 0b11110, 0b10000, 0b10000, 0b10000},
		'Q': {0b01110, 0b10001, 0b10001, 0b10001, 0b10101, 0b10010, 0b01101},
		'R': {0b11110, 0b10001, 0b10001, 0b11110, 0b10100, 0b10010, 0b10001},
		'S': {0b01111, 0b10000, 0b10000, 0b01110, 0b00001, 0b00001, 0b11110},
		'T': {0b11111, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100},
		'U': {0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
		'W': {0b10001, 0b10001, 0b10001, 0b10101, 0b10101, 0b11011, 0b10001},
		'X': {0b10001, 0b10001, 0b01010, 0b00100, 0b01010, 0b10001, 0b10001},
		'Y': {0b10001, 0b10001, 0b10001, 0b01010, 0b00100, 0b00100, 0b00100},
		'Z': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b10000, 0b11111},
		'2': {0b01110, 0b10001, 0b00001, 0b00010, 0b00100, 0b01000, 0b11111},
		'3': {0b11110, 0b00001, 0b00001, 0b01110, 0b00001, 0b00001, 0b11110},
		'4': {0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
		'5': {0b11111, 0b10000, 0b10000, 0b11110, 0b00001, 0b00001, 0b11110},
		'6': {0b01110, 0b10000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110},
		'7': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
		'8': {0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
		'9': {0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00001, 0b01110},
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
