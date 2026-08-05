package debugger

import "testing"

// Pure leaf-formatter tests: these take raw little-endian bytes plus an implied
// width and assert the rendered string, so they need no DWARF data or backend.

func TestFormatInt(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want string
	}{
		{"int8 -1", []byte{0xff}, "-1"},
		{"int8 127", []byte{0x7f}, "127"},
		{"int16 -2", []byte{0xfe, 0xff}, "-2"},
		{"int32 258", []byte{0x02, 0x01, 0x00, 0x00}, "258"},
		{"int64 -1", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "-1"},
		{"int64 max", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, "9223372036854775807"},
	}
	for _, c := range cases {
		if got := formatInt(c.buf); got != c.want {
			t.Errorf("%s: formatInt(%x) = %q, want %q", c.name, c.buf, got, c.want)
		}
	}
}

func TestFormatUint(t *testing.T) {
	cases := []struct {
		buf  []byte
		want string
	}{
		{[]byte{0xff}, "255"},
		{[]byte{0x00, 0x01}, "256"},
		{[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "18446744073709551615"},
	}
	for _, c := range cases {
		if got := formatUint(c.buf); got != c.want {
			t.Errorf("formatUint(%x) = %q, want %q", c.buf, got, c.want)
		}
	}
}

func TestFormatBool(t *testing.T) {
	if got := formatBool([]byte{0x00}); got != "false" {
		t.Errorf("formatBool(00) = %q, want false", got)
	}
	if got := formatBool([]byte{0x01}); got != "true" {
		t.Errorf("formatBool(01) = %q, want true", got)
	}
}

func TestFormatFloat(t *testing.T) {
	// 3.5 as float64 = 0x400C000000000000 (LE)
	f64 := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0c, 0x40}
	if got := formatFloat(f64); got != "3.5" {
		t.Errorf("formatFloat(f64 3.5) = %q, want 3.5", got)
	}
	// 1.5 as float32 = 0x3FC00000 (LE)
	f32 := []byte{0x00, 0x00, 0xc0, 0x3f}
	if got := formatFloat(f32); got != "1.5" {
		t.Errorf("formatFloat(f32 1.5) = %q, want 1.5", got)
	}
}

func TestFormatComplex(t *testing.T) {
	// complex128 (3 + 4i): re float64=3, im float64=4
	c128 := []byte{
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x08, 0x40, // 3.0
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x40, // 4.0
	}
	if got := formatComplex(c128); got != "(3+4i)" {
		t.Errorf("formatComplex(3+4i) = %q, want (3+4i)", got)
	}
	// complex64 (1 - 2i)
	c64 := []byte{
		0x00, 0x00, 0x80, 0x3f, // 1.0
		0x00, 0x00, 0x00, 0xc0, // -2.0
	}
	if got := formatComplex(c64); got != "(1-2i)" {
		t.Errorf("formatComplex(1-2i) = %q, want (1-2i)", got)
	}
}
