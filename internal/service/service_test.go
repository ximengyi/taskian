package service

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

func TestEncodePowerShellRoundTrip(t *testing.T) {
	want := `$a='C:\Program Files\Taskian\taskian.exe'`
	data, err := base64.StdEncoding.DecodeString(encodePowerShell(want))
	if err != nil {
		t.Fatal(err)
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	if got := string(utf16.Decode(units)); got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

func TestPSQuote(t *testing.T) {
	if got := psQuote(`C:\O'Brien\taskian.exe`); got != `C:\O''Brien\taskian.exe` {
		t.Fatalf("got=%q", got)
	}
}
