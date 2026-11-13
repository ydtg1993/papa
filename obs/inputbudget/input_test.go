package inputbudget

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLineExactAndPlusOne(t *testing.T) {
	for _, raw := range []string{"1234", "123\n", "12345", "1234\n"} {
		m := New(Limits{MaxLineBytes: 4, MaxBytes: 4})
		_, err := m.Line(context.Background(), bufio.NewReader(strings.NewReader(raw)))
		if errors.Is(err, ErrLimit) != (len(raw) > 4) {
			t.Fatalf("raw=%q err=%v", raw, err)
		}
	}
}
func TestLineCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(Limits{}).Line(ctx, bufio.NewReader(strings.NewReader("x")))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
